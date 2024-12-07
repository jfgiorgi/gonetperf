package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// this program measures the speed of the Golang http server (and client) using HTTP/1.1 (no tls)
// it also allows to test client context deadline and server timeouts behavior (context)
// usage: run with no argument to do a test on loopback interface with 10G of data
// use the "-s" option to be in server only mode and use another program like curl (or https://nspeed.app) to test
// locally or over the wire. you can also use the same program as a remote client with the "-c url" option
// use the "-t duration" to limit the test duration from the client side (which is on at 8 seconds by default)
// use the "-st duration" to limit the test duration from the server side.
// see "-h" also for more help.

// build a 1MiB buffer of random data
const MaxChunkSize = 1024 * 1024 // warning : 1 MiB // this will be allocated in memory
var BigChunk [MaxChunkSize]byte

var bigbuff [16 * 1024 * 1024]byte

func InitBigChunk(seed int64) {
	rng := rand.New(rand.NewSource(seed))
	for i := int64(0); i < MaxChunkSize; i++ {
		BigChunk[i] = byte(rng.Intn(256))
	}
}

func init() {
	InitBigChunk(time.Now().Unix())
}

// implements io.Discard
type Metrics struct {
	mu          sync.Mutex
	StepSize    int64
	StartTime   time.Time
	ElapsedTime time.Duration
	TotalRead   int64
	ReadCount   int64
}

// Write - performance sensitive, don't do much here
// it's basically a io.Discard with some metrics stored
func (wm *Metrics) Write(p []byte) (int, error) {
	n := len(p)
	s := int64(n)
	wm.mu.Lock()
	defer wm.mu.Unlock()

	// store bigest step size
	if s > wm.StepSize {
		wm.StepSize = s
	}

	wm.TotalRead += s

	// store elasped time
	if wm.ReadCount == 0 {
		wm.StartTime = time.Now()
	} else {
		wm.ElapsedTime = time.Since(wm.StartTime)
	}
	wm.ReadCount++
	return n, nil
}

// regexp to parse url
var StreamPathRegexp *regexp.Regexp

func createHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	var handler http.Handler = mux
	return handler
}

// handle the only route: '/number' which send <number> bytes of random data
func rootHandler(w http.ResponseWriter, r *http.Request) {

	//fmt.Printf("request from %s: %s\n", r.RemoteAddr, r.URL)
	method := r.Method
	if method == "" {
		method = "GET"
	}

	var timeout time.Duration = 0
	var err error
	// parse an optionnal "timeout" query parameter in Go time.Duration syntax
	if sTimeout := r.URL.Query().Get("timeout"); sTimeout != "" {
		timeout, err = time.ParseDuration(sTimeout)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}

	if method == "GET" {
		match := StreamPathRegexp.FindStringSubmatch(r.URL.Path[1:])
		if len(match) == 2 {
			n, err := strconv.ParseInt(match[1], 10, 64)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			streamBytes(w, r, n, timeout)
			return
		} else {
			http.Error(w, "Not found (no regexp match)", http.StatusNotFound)
			return
		}
	}
	if method == "POST" {
		startedAt := time.Now()
		fmt.Printf("%s - starting Upload (%s) of %d bytes from %s\n", startedAt.Format("2006-01-02 15:04:05"), r.Proto, r.ContentLength, r.RemoteAddr)
		n, err := io.CopyBuffer(io.Discard, r.Body, bigbuff[:])
		endedAt := time.Now()
		dur := endedAt.Sub(startedAt)
		if err != nil {
			http.Error(w, fmt.Sprintf("upload error %v", err), http.StatusInternalServerError)
		}
		report := fmt.Sprintf("%s - received %d bytes in %s (%s) with %s from  %s (expected %d bytes)\n",
			endedAt.Format("2006-01-02 15:04:05"),
			n,
			dur,
			FormatBitperSecond(dur.Seconds(), n),
			r.Proto, r.RemoteAddr, r.ContentLength)
		fmt.Println(report)
		w.Write([]byte(report))
		return

	}
	http.Error(w, "unhandled method", http.StatusBadRequest)
}

// send 'size' bytes of random data
func streamBytes(w http.ResponseWriter, r *http.Request, size int64, timeout time.Duration) {

	// the buffer we use to send data
	var chunkSize int64 = 256 * 1024 // 256KiB chunk (sweet spot value may depend on OS & hardware)
	if chunkSize > MaxChunkSize {
		log.Fatal("chunksize is too big")
	}
	chunk := BigChunk[:chunkSize]

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

	if timeout > 0 {
		rc := http.NewResponseController(w)
		err := rc.SetWriteDeadline(time.Now().Add(timeout))
		if err != nil {
			fmt.Printf("can't SetWriteDeadline: %s\n", err)
		}
	}

	startTime := time.Now()

	size_tx := int64(0)
	hasEnded := false
	var writeErr error
	var numChunk = size / chunkSize
	for i := int64(0); i < numChunk; i++ {
		n, err := w.Write(chunk)
		size_tx = size_tx + int64(n)
		if err != nil {
			hasEnded = true
			writeErr = err
		}
	}
	if size%chunkSize > 0 && !hasEnded {
		n, err := w.Write(chunk[:size%chunkSize])
		size_tx = size_tx + int64(n)
		if err != nil {
			writeErr = err
		}
	}

	// f := w.(http.Flusher)
	// f.Flush()

	duration := time.Since(startTime)
	fmt.Printf("[SERVER] sent %d bytes in %s = %s (%d chunks) to %s (server error : %s)\n", size_tx, duration, FormatBitperSecond(duration.Seconds(), size_tx), chunkSize, r.RemoteAddr, writeErr)
}

// create a H2/H3 HTTP server, wait for ctx.Done(), shutdown the server and signal the WaitGroup
func createServer(ctx context.Context, host string, port int, ipVersion int, wg *sync.WaitGroup, ready chan bool) {
	wg.Add(1)
	defer wg.Done()

	listenAddr := net.JoinHostPort(host, strconv.Itoa(port))
	server := &http.Server{
		Addr:    listenAddr,
		Handler: createHandler(),
	}

	networkTCP := "tcp"
	if ipVersion != 0 {
		networkTCP += strconv.Itoa(ipVersion)
	}
	ln, err := net.Listen(networkTCP, server.Addr)
	if err != nil {
		log.Fatalf("cannot listen (%s) to %s: %s", networkTCP, server.Addr, err)
	}

	go func() {
		<-ctx.Done()
		fmt.Printf("server %s shuting down\n", listenAddr)
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()

	// this will wait for ctx.Done then shutdown the server
	// signal the server is listening (so client(s) can start)
	ready <- true

	err = server.Serve(ln)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("cannot serve %s: %s", server.Addr, err)
	}
}

// http client, download the url to 'null' (discard)
func Download(ctx context.Context, url string, ipVersion int) error {

	//assert ipVersion = 0 || 4 || 6
	var dialer = &net.Dialer{
		Timeout:       1 * time.Second, // fail quick
		FallbackDelay: -1,              // don't use Happy Eyeballs
	}
	var netTransport = http.DefaultTransport.(*http.Transport).Clone()
	// custom DialContext that can force IPv4 or IPv6
	netTransport.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		if network == "udp" || network == "tcp" {
			if ipVersion != 0 {
				network += strconv.Itoa(ipVersion)
			}
		}
		return dialer.DialContext(ctx, network, address)
	}

	var rt http.RoundTripper = netTransport

	netTransport.ForceAttemptHTTP2 = false
	netTransport.TLSClientConfig.NextProtos = []string{"http/1.1"}

	trace := &httptrace.ClientTrace{
		GotConn: func(connInfo httptrace.GotConnInfo) {
			if connInfo.Conn != nil {
				fmt.Println("client connected to ", connInfo.Conn.RemoteAddr())
			}
		},
	}
	ctx = httptrace.WithClientTrace(ctx, trace)

	var body io.ReadCloser = http.NoBody
	req, err := http.NewRequestWithContext(ctx, "GET", url, body)

	if err != nil {
		return err
	}

	// client
	client := &http.Client{Transport: rt}
	resp, err := client.Do(req)

	if err == nil && resp != nil {
		fmt.Printf("receiving data with %s\n", resp.Proto)
		wm := Metrics{}
		_, err = io.CopyBuffer(&wm, resp.Body, bigbuff[:])

		resp.Body.Close()

		timedOut := "no"
		if errors.Is(err, io.EOF) {
			err = nil
			timedOut = "server timeout"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = nil

			timedOut = "client timeout"
		}
		if err != nil {
			return err
		}
		fmt.Printf("client received %d bytes in %v = %s, %d write ops, %d buff (timeout: %s)\n", wm.TotalRead, wm.ElapsedTime, FormatBitperSecond(wm.ElapsedTime.Seconds(), wm.TotalRead), wm.ReadCount, wm.StepSize, timedOut)
	}
	return err
}

// client just like "curl -o /dev/null url"
func doClient(ctx context.Context, url string, timeout time.Duration, ipVersion int) error {
	fmt.Printf("downloading %s\n", url)
	if timeout > 0 {
		ctx2, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ctx = ctx2
	}
	err := Download(ctx, url, ipVersion)
	if err != nil {
		fmt.Printf("client error for %s: %s\n", url, err)
	}
	return err
}

var optServer = flag.Bool("s", false, "server mode only")
var optClient = flag.String("c", "", "client only mode, connect to url")

var optCpuProfile = flag.String("cpuprof", "", "write cpu profile to file")

var optSize = flag.Uint64("b", 10000000000, "number of bytes to transfert")
var optTimeout = flag.Duration("t", 8*time.Second, "client timeout (in golang duration)")
var optSTimeout = flag.Duration("st", 0, "server timeout (in golang duration)")

var optIPv4 = flag.Bool("4", false, "force IPv4")
var optIPv6 = flag.Bool("6", false, "force IPv6")

func main() {

	flag.Parse()
	ipVersion := 0

	if *optIPv4 && *optIPv6 {
		log.Fatal("cant force both IPv4 and IPv6")
	}
	if *optIPv4 {
		ipVersion = 4
	}
	if *optIPv6 {
		ipVersion = 6
	}

	if *optCpuProfile != "" {
		runtime.SetBlockProfileRate(1)
		f, err := os.Create(*optCpuProfile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer func() {
			pprof.StopCPUProfile()
		}()
	}

	StreamPathRegexp = regexp.MustCompile("^(" + "[0-9]+" + ")$")
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup

	if *optClient == "" {
		ready := make(chan bool)

		go createServer(ctx, "", 2222, ipVersion, &wg, ready)
		<-ready
		// if server mode, just wait forever for something else to cancel
		if *optServer {
			fmt.Printf("server mode on\n")
			<-ctx.Done()
			return
		}
	} else {
		doClient(ctx, *optClient, *optTimeout, ipVersion)
		return
	}
	hostname := "localhost"
	// the Go dns resolver has some issue with localhost & IPv6
	if ipVersion == 6 {
		hostname = "[::1]"
	}
	url := fmt.Sprintf("http://%s:2222/%d", hostname, *optSize)
	if *optSTimeout > 0 {
		url += "?timeout=" + (*optSTimeout).String()
	}
	doClient(ctx, url, *optTimeout, ipVersion)
	fmt.Println()
	cancel()
	wg.Wait()
}

// human friendly formatting stuff

// FormatBitperSecond format bit per seconds in human readable format
func FormatBitperSecond(elapsedSeconds float64, totalBytes int64) string {
	// nyi - fix me
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("recovered from divide by zero")
		}
	}()
	speed := "(too fast)"
	if elapsedSeconds > 0 {
		speed = ByteCountDecimal((int64)(((float64)(totalBytes)*8.0)/elapsedSeconds)) + "bps"
	}
	return speed
}

// ByteCountDecimal format byte size to human readable format (decimal units)
// suitable to append the unit name after (B, bps, etc)
func ByteCountDecimal(b int64) string {
	s, u := byteCount(b, 1000, "kMGTPE")
	return s + " " + u
}

// copied from : https://programming.guide/go/formatting-byte-size-to-human-readable-format.html
func byteCount(b int64, unit int64, units string) (string, string) {
	if b < unit {
		return fmt.Sprintf("%d", b), ""
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	if exp >= len(units) {
		return fmt.Sprintf("%d", b), ""
	}
	return fmt.Sprintf("%.1f", float64(b)/float64(div)), units[exp : exp+1]
}
