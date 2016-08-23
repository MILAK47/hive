package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsouza/go-dockerclient"
	"gopkg.in/inconshreveable/log15.v2"
)

// benchmarkClients runs a batch of benchmark tests matched by benchmarkerPattern
// against all clients matching clientPattern.
func benchmarkClients(daemon *docker.Client, clientPattern, benchmarkerPattern string, overrides []string) error {
	// Build all the clients matching the benchmark pattern
	log15.Info("building clients for benchmark", "pattern", clientPattern)
	clients, err := buildClients(daemon, clientPattern)
	if err != nil {
		return err
	}
	// Build all the benchmarkers known to the harness
	log15.Info("building benchmarkers for measurements", "pattern", benchmarkerPattern)
	benchmarkers, err := buildBenchmarkers(daemon, benchmarkerPattern)
	if err != nil {
		return err
	}
	// Iterate over all client and benchmarker combos and cross-execute them
	results := make(map[string]map[string]map[string]int64)

	for client, clientImage := range clients {
		results[client] = make(map[string]map[string]int64)

		for benchmarker, benchmarkerImage := range benchmarkers {
			// Create the logger and log folder
			logger := log15.New("client", client, "benchmarker", benchmarker)

			logdir := filepath.Join(hiveLogsFolder, "benchmarks", fmt.Sprintf("%s[%s]", strings.Replace(benchmarker, "/", ":", -1), client))
			os.RemoveAll(logdir)

			// Wrap the benchmark code into the Go's testing framework
			result := testing.Benchmark(func(b *testing.B) {
				if pass, err := benchmark(daemon, clientImage, benchmarkerImage, overrides, logger, logdir, b); err != nil {
					b.Fatalf("benchmark failed: %v", err)
				} else if !pass {
					b.Fatalf("benchmark failed")
				}
			})
			results[client][benchmarker] = make(map[string]int64)
			results[client][benchmarker]["iters"] = int64(result.N)
			results[client][benchmarker]["ns/op"] = result.NsPerOp()
		}
	}
	// Print the benchmark logs
	out, _ := json.MarshalIndent(results, "", "  ")
	fmt.Printf("Benchmark results:\n%s\n", string(out))

	return nil
}

func benchmark(daemon *docker.Client, client, benchmarker string, overrides []string, logger log15.Logger, logdir string, b *testing.B) (bool, error) {
	logger.Info("running client benchmark", "iterations", b.N)

	// Create the client container and make sure it's cleaned up afterwards
	logger.Debug("creating client container")
	cc, err := createClientContainer(daemon, client, benchmarker, nil, overrides, nil)
	if err != nil {
		logger.Error("failed to create client", "error", err)
		return false, err
	}
	clogger := logger.New("id", cc.ID[:8])
	clogger.Debug("created client container")
	defer func() {
		clogger.Debug("deleting client container")
		daemon.RemoveContainer(docker.RemoveContainerOptions{ID: cc.ID, Force: true})
	}()

	// Start the client container and retrieve its IP address for the benchmarker
	clogger.Debug("running client container")
	cwaiter, err := runContainer(daemon, cc.ID, clogger, filepath.Join(logdir, "client.log"), false)
	if err != nil {
		clogger.Error("failed to run client", "error", err)
		return false, err
	}
	defer cwaiter.Close()

	lcc, err := daemon.InspectContainer(cc.ID)
	if err != nil {
		clogger.Error("failed to retrieve client IP", "error", err)
		return false, err
	}
	cip := lcc.NetworkSettings.IPAddress

	// Wait for the HTTP/RPC socket to open or the container to fail
	start := time.Now()
	for {
		// If the container died, bail out
		c, err := daemon.InspectContainer(cc.ID)
		if err != nil {
			clogger.Error("failed to inspect client", "error", err)
			return false, err
		}
		if !c.State.Running {
			clogger.Error("client container terminated")
			return false, errors.New("terminated unexpectedly")
		}
		// Container seems to be alive, check whether the RPC is accepting connections
		if conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", c.NetworkSettings.IPAddress, 8545)); err == nil {
			clogger.Debug("client container online", "time", time.Since(start))
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Start the benchmark API server to provide access to the benchmark oracle
	bench, err := startBenchmarkerAPI(logger, b)
	if err != nil {
		logger.Error("failed to start benchmarker API", "error", err)
		return false, err
	}
	defer bench.Close()

	// Create the benchmarker container and make sure it's cleaned up afterwards
	logger.Debug("creating benchmarker container")
	vc, err := daemon.CreateContainer(docker.CreateContainerOptions{
		Config: &docker.Config{
			Image: benchmarker,
			Env: []string{
				"HIVE_CLIENT_IP=" + cip,
				"HIVE_BENCHMARKER=http://" + bench.listener.Addr().String(),
			},
		},
	})
	if err != nil {
		logger.Error("failed to create benchmarker", "error", err)
		return false, err
	}
	blogger := logger.New("id", vc.ID[:8])
	blogger.Debug("created benchmarker container")
	defer func() {
		blogger.Debug("deleting benchmarker container")
		daemon.RemoveContainer(docker.RemoveContainerOptions{ID: vc.ID, Force: true})
	}()

	// Start the tester container and wait until it finishes
	blogger.Debug("running benchmarker container")

	b.ResetTimer()
	bwaiter, err := runContainer(daemon, vc.ID, blogger, filepath.Join(logdir, "benchmarker.log"), false)
	if err != nil {
		blogger.Error("failed to run benchmarker", "error", err)
		return false, err
	}
	bwaiter.Wait()
	b.StopTimer()

	// Retrieve the exist status to report pass of fail
	v, err := daemon.InspectContainer(vc.ID)
	if err != nil {
		blogger.Error("failed to inspect benchmarker", "error", err)
		return false, err
	}
	return v.State.ExitCode == 0, nil
}

// startBenchmarkerAPI starts an HTTP webserver listening for benchmarker commands
// on the docker bridge and executing them until it is torn down.
func startBenchmarkerAPI(logger log15.Logger, b *testing.B) (*benchmarkerAPIHandler, error) {
	// Find the IP address of the host container
	logger.Debug("looking up docker bridge IP")
	bridge, err := lookupBridgeIP(logger)
	if err != nil {
		logger.Error("failed to lookup bridge IP", "error", err)
		return nil, err
	}
	logger.Debug("docker bridge IP found", "ip", bridge)

	// Start a tiny API webserver for benchmarkers to coordinate with
	logger.Debug("opening TCP socket for benchmarker")

	addr, _ := net.ResolveTCPAddr("tcp4", fmt.Sprintf("%s:0", bridge))
	listener, err := net.ListenTCP("tcp4", addr)
	if err != nil {
		logger.Error("failed to listen on bridge adapter", "error", err)
		return nil, err
	}
	logger.Debug("listening for benchmarker commands", "ip", bridge, "port", listener.Addr().(*net.TCPAddr).Port)

	// Serve connections until the listener is terminated
	logger.Debug("starting benchmarker API server")
	api := &benchmarkerAPIHandler{
		listener: listener,
		oracle:   b,
		logger:   logger,
	}
	go http.Serve(listener, api)

	return api, nil
}

// benchmarkerAPIHandler is the HTTP request handler directing the docker engine
// with the commands from the benchmarker runner.
type benchmarkerAPIHandler struct {
	listener *net.TCPListener
	oracle   *testing.B
	logger   log15.Logger
	autoID   uint32
}

// ServeHTTP handles all the benchmarker API requests and executes them.
func (h *benchmarkerAPIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	logger := h.logger.New("req-id", atomic.AddUint32(&h.autoID, 1))
	logger.Debug("new benchmarker request", "from", r.RemoteAddr, "method", r.Method, "endpoint", r.URL.Path)

	switch r.Method {
	case "GET":
		switch r.URL.Path {
		case "/iters":
			// Return the number of iterations required for this run
			fmt.Fprintf(w, "%d", h.oracle.N)

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}

	case "POST":
		switch r.URL.Path {
		case "/reset":
			// Restart the benchmark measurements, discarding all counters and timers
			h.oracle.ResetTimer()

		case "/stop":
			// Terminates the measurements but allows cleanups to still run
			h.oracle.StopTimer()

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Close terminates all running containers and tears down the API server.
func (h *benchmarkerAPIHandler) Close() {
	h.logger.Debug("terminating benchmarker server")
	h.listener.Close()
}
