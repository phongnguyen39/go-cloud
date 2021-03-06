// Copyright 2019 The Go Cloud Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
)

func serve(ctx context.Context, pctx *processContext, args []string) error {
	f := newFlagSet(pctx, "serve")
	opts := new(serveOptions)
	f.StringVar(&opts.address, "address", "localhost:8080", "`host:port` address to serve on")
	f.StringVar(&opts.biome, "biome", "dev", "`name` of biome to apply and use configuration from")
	if err := f.Parse(args); xerrors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return usagef("gocdk serve: %w", err)
	}
	if f.NArg() != 0 {
		return usagef("gocdk serve [options]")
	}

	// Check first that we're in a Go module.
	var err error
	opts.moduleRoot, err = findModuleRoot(ctx, pctx.workdir)
	if err != nil {
		return xerrors.Errorf("gocdk serve: %w", err)
	}

	// Verify that biome configuration permits serving.
	biomeConfig, err := readBiomeConfig(opts.moduleRoot, opts.biome)
	if xerrors.As(err, new(*biomeNotFoundError)) {
		// TODO(light): Keep err in formatting chain for debugging.
		return xerrors.Errorf("gocdk serve: biome configuration not found for %s. "+
			"Make sure that %s exists and has `\"serve_enabled\": true`.",
			opts.biome, filepath.Join(findBiomeDir(opts.moduleRoot, opts.biome), biomeConfigFileName))
	}
	if err != nil {
		return xerrors.Errorf("gocdk serve: %w", err)
	}
	if biomeConfig.ServeEnabled == nil || !*biomeConfig.ServeEnabled {
		return xerrors.Errorf("gocdk serve: biome %s has not enabled serving. "+
			"Add `\"serve_enabled\": true` to %s and try again.",
			opts.biome, filepath.Join(findBiomeDir(opts.moduleRoot, opts.biome), biomeConfigFileName))
	}

	// Start reverse proxy on address.
	proxyListener, err := net.Listen("tcp", opts.address)
	if err != nil {
		return xerrors.Errorf("gocdk serve: %w", err)
	}
	opts.actualAddress = proxyListener.Addr().(*net.TCPAddr)
	myProxy := new(serveProxy)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return runHTTPServer(groupCtx, proxyListener, myProxy)
	})

	// Start main build loop.
	logger := log.New(pctx.stderr, "gocdk: ", log.Ldate|log.Ltime)
	group.Go(func() error {
		return serveBuildLoop(groupCtx, pctx, logger, myProxy, opts)
	})
	if err := group.Wait(); err != nil {
		return xerrors.Errorf("gocdk serve: %w", err)
	}
	return nil
}

type serveOptions struct {
	moduleRoot string
	biome      string
	address    string

	// actualAddress is the local address that the reverse proxy is
	// listening on.
	actualAddress *net.TCPAddr
}

// serveBuildLoop builds and runs the user's server and sets the proxy's
// backend whenever a new built server becomes healthy. This loop will continue
// until ctx's Done channel is closed. serveBuildLoop returns an error only
// if it was unable to start the main build loop.
func serveBuildLoop(ctx context.Context, pctx *processContext, logger *log.Logger, myProxy *serveProxy, opts *serveOptions) error {
	// Listen for SIGUSR1 to trigger rebuild.
	reload, reloadDone := notifyUserSignal1()
	defer reloadDone()

	// Log biome that is being used.
	logger.Printf("Preparing to serve %s...", opts.biome)

	// Create a temporary build directory.
	buildDir, err := ioutil.TempDir("", "gocdk-build")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(buildDir); err != nil {
			logger.Printf("Cleaning build: %v", err)
		}
	}()

	// Apply Terraform configuration in biome.
	if err := apply(ctx, pctx, []string{opts.biome}); err != nil {
		return err
	}

	// Build and run the server.
	allocA := &serverAlloc{
		exePath: filepath.Join(buildDir, "serverA"),
		port:    opts.actualAddress.Port + 1,
	}
	allocB := &serverAlloc{
		exePath: filepath.Join(buildDir, "serverB"),
		port:    opts.actualAddress.Port + 2,
	}
	spareAlloc, liveAlloc := allocA, allocB
	var process *exec.Cmd
loop:
	for first := true; ; first = false {
		// After the first iteration of the loop, each iteration should wait for a
		// change in the filesystem before proceeding.
		if !first {
			// TODO(#1881): Actually check filesystem instead of SIGUSR1.
			select {
			case <-reload:
			case <-ctx.Done():
				break loop
			}
		}

		// Build and run the server.
		logger.Println("Building server...")
		if err := buildForServe(ctx, pctx, opts.moduleRoot, spareAlloc.exePath); err != nil {
			logger.Printf("Build: %v", err)
			if process == nil {
				myProxy.setBuildError(err)
			}
			continue
		}
		newProcess, err := spareAlloc.start(ctx, pctx, logger, opts.moduleRoot)
		if err != nil {
			logger.Printf("Starting server: %v", err)
			if process == nil {
				myProxy.setBuildError(err)
			}
			continue
		}

		// Server started successfully. Cut traffic over.
		myProxy.setBackend(spareAlloc.url(""))
		if process == nil {
			// First time server came up healthy: log greeting message to user.
			proxyURL := "http://" + formatTCPAddr(opts.actualAddress) + "/"
			logger.Printf("Serving at %s\nUse Ctrl-C to stop", proxyURL)
		} else {
			// Iterative build complete, kill old server.
			logger.Print("Reload complete.")
			endServerProcess(process)
		}
		process = newProcess
		spareAlloc, liveAlloc = liveAlloc, spareAlloc
	}
	logger.Println("Shutting down...")
	if process != nil {
		endServerProcess(process)
	}
	return nil
}

// buildForServe runs Wire and `go build` at moduleRoot to create exePath.
func buildForServe(ctx context.Context, pctx *processContext, moduleRoot string, exePath string) error {
	moduleEnv := pctx.overrideEnv("GO111MODULE=on")

	if wireExe, err := exec.LookPath("wire"); err == nil {
		// TODO(light): Only run Wire if needed, but that requires source analysis.
		wireCmd := exec.CommandContext(ctx, wireExe, "./...")
		wireCmd.Dir = moduleRoot
		wireCmd.Env = moduleEnv
		// TODO(light): Collect build logs into error.
		wireCmd.Stdout = pctx.stderr
		wireCmd.Stderr = pctx.stderr
		if err := wireCmd.Run(); err != nil {
			return xerrors.Errorf("build server: wire: %w", err)
		}
	}

	buildCmd := exec.CommandContext(ctx, "go", "build", "-o", exePath)
	buildCmd.Dir = moduleRoot
	buildCmd.Env = moduleEnv
	// TODO(light): Collect build logs into error.
	buildCmd.Stdout = pctx.stderr
	buildCmd.Stderr = pctx.stderr
	if err := buildCmd.Run(); err != nil {
		return xerrors.Errorf("build server: go build: %w", err)
	}

	return nil
}

// serverAlloc stores the built executable path and port for a single instance
// of the user's application server.
type serverAlloc struct {
	exePath string
	port    int
}

// url returns the URL to connect to the server with.
func (alloc *serverAlloc) url(path string) *url.URL {
	if path == "" {
		path = "/"
	}
	return &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("localhost:%d", alloc.port),
		Path:   path,
	}
}

// start starts the server process specified by the alloc and waits for
// it to become healthy.
func (alloc *serverAlloc) start(ctx context.Context, pctx *processContext, logger *log.Logger, workdir string) (*exec.Cmd, error) {
	// Run server.
	logger.Print("Starting server...")
	process := exec.Command(alloc.exePath)
	process.Dir = workdir
	process.Env = pctx.overrideEnv("PORT=" + strconv.Itoa(alloc.port))
	process.Stdout = pctx.stdout
	process.Stderr = pctx.stderr
	if err := process.Start(); err != nil {
		return nil, xerrors.Errorf("start server: %w", err)
	}

	// Server must report alive within 30 seconds.
	// TODO(light): Also wait on process to see if it exits early.
	aliveCtx, aliveCancel := context.WithTimeout(ctx, 30*time.Second)
	err := waitForHealthy(aliveCtx, alloc.url("/healthz/liveness"))
	aliveCancel()
	if err != nil {
		process.Process.Kill() // Send SIGKILL; no graceful shutdown.
		process.Wait()
		return nil, xerrors.Errorf("start server: %w", err)
	}

	// Wait for server to be ready.
	logger.Printf("Waiting for server %s to report ready...", alloc.url("/"))
	err = waitForHealthy(ctx, alloc.url("/healthz/readiness"))
	if err != nil {
		endServerProcess(process)
		return nil, xerrors.Errorf("start server: %w", err)
	}
	return process, nil
}

// runHTTPServer runs an HTTP server until ctx's Done channel is closed.
// It returns an error only if the server returned an error before the Done
// channel was closed.
func runHTTPServer(ctx context.Context, l net.Listener, handler http.Handler) error {
	server := &http.Server{
		Handler: handler,
	}
	serverDone := make(chan error)
	go func() {
		serverDone <- server.Serve(l)
	}()
	select {
	case err := <-serverDone:
		return err
	case <-ctx.Done():
		// Don't pass ctx, since this context determines the shutdown timeout.
		server.Shutdown(context.Background())
		<-serverDone
		return nil
	}
}

// serveProxy is a reverse HTTP proxy that can hot-swap to other sources.
// The zero value will serve Bad Gateway responses until setBackend or
// setBuildError is called.
type serveProxy struct {
	backend atomic.Value
}

// setBackend serves any future requests by reverse proxying to the given URL.
func (p *serveProxy) setBackend(target *url.URL) {
	p.backend.Store(httputil.NewSingleHostReverseProxy(target))
}

// setBuildError serves any future requests by serving an Internal Server Error
// with the error's message as the body.
func (p *serveProxy) setBuildError(e error) {
	p.backend.Store(e)
}

func (p *serveProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch b := p.backend.Load().(type) {
	case nil:
		http.Error(w, "waiting for initial build...", http.StatusBadGateway)
	case error:
		http.Error(w, b.Error(), http.StatusInternalServerError)
	case http.Handler:
		b.ServeHTTP(w, r)
	default:
		panic("unreachable")
	}
}

// waitForHealthy polls a URL repeatedly until the server responds with a
// non-server-error status, the context is canceled, or the context's deadline
// is met. waitForHealthy returns an error in the latter two cases.
func waitForHealthy(ctx context.Context, u *url.URL) error {
	// Create health request.
	// (Avoiding http.NewRequest to not reparse the URL.)
	req := &http.Request{
		Method: http.MethodGet,
		URL:    u,
	}
	req = req.WithContext(ctx)

	// Poll for healthy.
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		// Check response. Allow 200-level (success) or 400-level (client error)
		// status codes. The latter is permitted for the case where the application
		// doesn't serve explicit health checks.
		resp, err := http.DefaultClient.Do(req)
		if err == nil && (statusCodeInRange(resp.StatusCode, 200) || statusCodeInRange(resp.StatusCode, 400)) {
			return nil
		}
		// Wait for the next tick.
		select {
		case <-tick.C:
		case <-ctx.Done():
			return xerrors.Errorf("wait for healthy: %w", ctx.Err())
		}
	}
}

// endServerProcess kills and waits for a subprocess to exit.
func endServerProcess(process *exec.Cmd) {
	if err := signalGracefulShutdown(process.Process); err != nil {
		process.Process.Kill()
	}
	process.Wait()
}

// formatTCPAddr converts addr into a "host:port" string usable for a
// URL.
func formatTCPAddr(addr *net.TCPAddr) string {
	if addr.IP.IsUnspecified() {
		return fmt.Sprintf("localhost:%d", addr.Port)
	}
	return addr.String()
}

// statusCodeInRange reports whether the given HTTP status code is in the range
// [start, start+100).
func statusCodeInRange(statusCode, start int) bool {
	if start < 100 || start%100 != 0 {
		panic("statusCodeInRange start must be a multiple of 100")
	}
	return start <= statusCode && statusCode < start+100
}
