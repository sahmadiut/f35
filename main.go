package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// version is injected at build time via ldflags: -X main.version=<tag>
var version = "dev"

type Config struct {
	Engine          string
	ClientPath      string
	Domain          string
	ResolversFile   string
	TestURL         string
	DownloadURL     string
	OutputFile      string
	JSON            bool
	Quiet           bool
	Probe           bool
	Download        bool
	Whois           bool
	Background      bool
	Version         bool
	Proxy           string
	ProxyUser       string
	ProxyPass       string
	Args            string
	ParsedArgs      []string
	Colorize        bool
	Workers         int
	Retries         int
	TunnelWait      int
	Timeout         int
	DownloadTimeout int
	WhoisTimeout    int
	StartPort       int
}

type EngineSpec struct {
	DefaultBinary        string
	DefaultArgs          []string
	InsertArgsBeforeTail bool
}

var engineSpecs = map[string]EngineSpec{
	"dnstt": {
		DefaultBinary: "dnstt-client",
		DefaultArgs: []string{
			"-udp", "{resolver}",
			"{domain}",
			"{listen}",
		},
		InsertArgsBeforeTail: true,
	},
	"slipstream": {
		DefaultBinary: "slipstream-client",
		DefaultArgs: []string{
			"--tcp-listen-host", "{listen_host}",
			"--tcp-listen-port", "{listen_port}",
			"--resolver", "{resolver}",
			"--domain", "{domain}",
			"--keep-alive-interval", "200",
		},
	},
	"vaydns": {
		DefaultBinary: "vaydns-client",
		DefaultArgs: []string{
			"-domain", "{domain}",
			"-listen", "{listen}",
			"-udp", "{resolver}",
		},
	},
}

const whoisURL = "https://api.ipiz.net"

type whoisResponse struct {
	OrgName string `json:"org_name"`
	Country string `json:"country"`
	Status  string `json:"status"`
}

type Result struct {
	Resolver          string  `json:"resolver"`
	LatencyMS         int64   `json:"latency_ms"`
	Probe             string  `json:"probe"`
	Whois             string  `json:"whois"`
	Download          string  `json:"download"`
	DownloadSpeedKBps float64 `json:"download_speed_kbps,omitempty"`
	Org               string  `json:"org,omitempty"`
	Country           string  `json:"country,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags()

	if cfg.Version {
		fmt.Printf("f35 version %s\n", version)
		return nil
	}

	if err := validateConfig(cfg); err != nil {
		flag.Usage()
		return err
	}

	// Background mode: re-exec self without -bg, redirect output to file.
	if cfg.Background {
		return runInBackground(cfg.OutputFile)
	}

	resolvers, err := loadResolvers(cfg.ResolversFile)
	if err != nil {
		return err
	}

	total := len(resolvers)

	// Open output file for saving found resolver IPs.
	var outFile *os.File
	var outMu sync.Mutex
	if cfg.OutputFile != "" {
		outFile, err = os.OpenFile(cfg.OutputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("cannot open output file: %w", err)
		}
		defer outFile.Close()
	}

	stderrTerm := stderrIsTerminal()

	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "f35 %s | scan started: resolvers=%d workers=%d engine=%s\n",
			version, total, cfg.Workers, cfg.Engine)
		if cfg.OutputFile != "" {
			fmt.Fprintf(os.Stderr, "output: %s\n", cfg.OutputFile)
		}
	}

	jobs := make(chan string, cfg.Workers*2)
	var wg sync.WaitGroup
	var working atomic.Int64
	var scanned atomic.Int64

	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			worker(port, cfg, jobs, &working, &scanned, total, outFile, &outMu, stderrTerm)
		}(cfg.StartPort + i)
	}

	for _, r := range resolvers {
		jobs <- r
	}
	close(jobs)

	wg.Wait()
	if !cfg.Quiet {
		if stderrTerm {
			fmt.Fprintf(os.Stderr, "\r\033[K")
		}
		fmt.Fprintf(os.Stderr, "scan finished: scanned=%d working=%d\n", total, working.Load())
	}
	return nil
}

// runInBackground re-executes the binary without -bg, detaches it from the
// terminal, and redirects stdout+stderr to the output file.
func runInBackground(outputFile string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}

	// Build args without -bg / --bg.
	var newArgs []string
	for _, arg := range os.Args[1:] {
		if arg == "-bg" || arg == "--bg" {
			continue
		}
		newArgs = append(newArgs, arg)
	}

	outPath := outputFile
	if outPath == "" {
		outPath = "f35-results.txt"
	}

	// Make sure -o is present in the forwarded args so the child writes to file.
	hasO := false
	for i, a := range newArgs {
		if a == "-o" && i+1 < len(newArgs) {
			hasO = true
			break
		}
	}
	if !hasO {
		newArgs = append(newArgs, "-o", outPath)
	}

	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("cannot open output file %s: %w", outPath, err)
	}

	cmd := exec.Command(exe, newArgs...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	detachProcess(cmd)

	if err := cmd.Start(); err != nil {
		f.Close()
		return fmt.Errorf("failed to start background process: %w", err)
	}

	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	f.Close()

	fmt.Printf("f35 is running in the background\n")
	fmt.Printf("  PID:    %d\n", pid)
	fmt.Printf("  Output: %s\n", outPath)
	fmt.Printf("\nTo stop: kill %d\n", pid)
	return nil
}

func parseFlags() *Config {
	c := &Config{}

	flag.BoolVar(&c.Version, "v", false, "Print version information and exit")
	flag.StringVar(&c.ResolversFile, "r", "", "Path to file containing resolvers (IP or IP:PORT per line)")
	flag.StringVar(&c.Engine, "e", "dnstt", fmt.Sprintf("Tunnel engine to use: %s", strings.Join(engineNames(), "|")))
	flag.StringVar(&c.ClientPath, "p", "", "Explicit path to client binary (optional)")
	flag.StringVar(&c.Domain, "d", "", "Tunnel domain (e.g., ns.example.com)")
	flag.StringVar(&c.Args, "a", "", "Extra engine CLI args; supports placeholders like {resolver}, {domain}, {listen}")
	flag.BoolVar(&c.JSON, "json", false, "Print one JSON object per result line")
	flag.BoolVar(&c.Quiet, "q", false, "Suppress startup and completion logs")
	flag.StringVar(&c.TestURL, "u", "http://www.google.com/gen_204", "HTTP URL used for the probe request through the tunnel")
	flag.BoolVar(&c.Probe, "probe", true, "Run a quick connectivity probe through the tunnel")
	flag.BoolVar(&c.Download, "download", false, "Run a real download test through the tunnel")
	flag.StringVar(&c.DownloadURL, "download-url", "https://speed.cloudflare.com/__down?bytes=100000", "HTTP URL used for the download test")
	flag.BoolVar(&c.Whois, "whois", false, "Lookup resolver owner info and print organization and country")
	flag.StringVar(&c.Proxy, "x", "socks5h", "Protocol to use when sending request through the tunnel: http|https|socks5|socks5h")
	flag.StringVar(&c.ProxyUser, "U", "", "Proxy username (if the tunnel exit requires auth)")
	flag.StringVar(&c.ProxyPass, "P", "", "Proxy password (if the tunnel exit requires auth)")
	flag.IntVar(&c.Workers, "w", 20, "Number of concurrent scanning workers")
	flag.IntVar(&c.Retries, "R", 0, "Number of retries per resolver after the first failure")
	flag.IntVar(&c.TunnelWait, "s", 1000, "Time to wait (ms) for tunnel establishment before testing HTTP")
	flag.IntVar(&c.Timeout, "t", 5, "Probe request timeout in seconds")
	flag.IntVar(&c.DownloadTimeout, "download-timeout", 5, "Download request timeout in seconds")
	flag.IntVar(&c.WhoisTimeout, "whois-timeout", 15, "WHOIS lookup timeout in seconds")
	flag.IntVar(&c.StartPort, "l", 40000, "Starting local port for tunnel listeners")
	flag.StringVar(&c.OutputFile, "o", "f35-results.txt", "Output file path for found resolvers (one IP:port per line)")
	flag.BoolVar(&c.Background, "bg", false, "Run in background (detach from terminal); output goes to -o file")

	flag.Parse()

	c.Engine = strings.ToLower(strings.TrimSpace(c.Engine))
	c.Proxy = strings.ToLower(strings.TrimSpace(c.Proxy))
	c.Colorize = !c.JSON && stdoutIsTerminal()
	return c
}

func validateConfig(cfg *Config) error {
	if cfg.ResolversFile == "" || cfg.Domain == "" {
		return errors.New("-r and -d are required")
	}

	spec, ok := engineSpecs[cfg.Engine]
	if !ok {
		return fmt.Errorf("-e must be one of: %s", strings.Join(engineNames(), ", "))
	}

	if cfg.Args != "" {
		parsedArgs, err := splitCommandLine(cfg.Args)
		if err != nil {
			return fmt.Errorf("invalid -a: %w", err)
		}
		cfg.ParsedArgs = parsedArgs
	}

	switch cfg.Proxy {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("-x must be one of: http, https, socks5, socks5h")
	}

	if cfg.ProxyPass != "" && cfg.ProxyUser == "" {
		return errors.New("-P requires -U")
	}
	if !cfg.Probe && !cfg.Download && !cfg.Whois {
		return errors.New("at least one of -probe, -download, or -whois must be enabled")
	}

	if cfg.Workers < 1 {
		return errors.New("-w must be >= 1")
	}
	if cfg.Retries < 0 {
		return errors.New("-R must be >= 0")
	}
	if cfg.Timeout < 1 {
		return errors.New("-t must be >= 1")
	}
	if cfg.DownloadTimeout < 1 {
		return errors.New("--download-timeout must be >= 1")
	}
	if cfg.WhoisTimeout < 1 {
		return errors.New("--whois-timeout must be >= 1")
	}
	if cfg.TunnelWait < 0 {
		return errors.New("-s must be >= 0")
	}
	if cfg.StartPort < 1 || cfg.StartPort > 65535 {
		return errors.New("-l must be between 1 and 65535")
	}
	if cfg.StartPort+cfg.Workers-1 > 65535 {
		return errors.New("port range overflow (-l + -w exceeds 65535)")
	}

	if cfg.ClientPath == "" {
		path, err := exec.LookPath(spec.DefaultBinary)
		if err != nil {
			return fmt.Errorf("binary %s not found in PATH; use -p to specify path", spec.DefaultBinary)
		}
		cfg.ClientPath = path
	}
	return nil
}

func engineNames() []string {
	names := make([]string, 0, len(engineSpecs))
	for name := range engineSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func loadResolvers(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	seen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		addr, ok := formatAddr(line)
		if ok && !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no valid resolvers found")
	}
	return out, sc.Err()
}

func formatAddr(line string) (string, bool) {
	if ip := net.ParseIP(line); ip != nil {
		return net.JoinHostPort(ip.String(), "53"), true
	}
	host, port, err := net.SplitHostPort(line)
	if err != nil || net.ParseIP(host) == nil {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

func worker(port int, cfg *Config, jobs <-chan string, working *atomic.Int64,
	scanned *atomic.Int64, total int, outFile *os.File, outMu *sync.Mutex, stderrTerm bool) {
	proxyURL := &url.URL{
		Scheme: cfg.Proxy,
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
	}
	if cfg.ProxyUser != "" {
		if cfg.ProxyPass != "" {
			proxyURL.User = url.UserPassword(cfg.ProxyUser, cfg.ProxyPass)
		} else {
			proxyURL.User = url.User(cfg.ProxyUser)
		}
	}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			DisableKeepAlives: true,
		},
	}

	for resolver := range jobs {
		for i := 0; i <= cfg.Retries; i++ {
			if try(resolver, port, cfg, client, outFile, outMu) {
				working.Add(1)
				break
			}
		}
		s := scanned.Add(1)
		if !cfg.Quiet {
			w := working.Load()
			remaining := int64(total) - s
			progress := fmt.Sprintf("[%d/%d] working=%d remaining=%d", s, total, w, remaining)
			if stderrTerm {
				fmt.Fprintf(os.Stderr, "\r\033[K%s", progress)
			} else {
				fmt.Fprintf(os.Stderr, "%s\n", progress)
			}
		}
	}
}

func try(resolver string, port int, cfg *Config, client *http.Client, outFile *os.File, outMu *sync.Mutex) bool {
	args, err := buildEngineArgs(cfg, resolver, port)
	if err != nil {
		return false
	}

	cmd := exec.Command(cfg.ClientPath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return false
	}

	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	time.Sleep(time.Duration(cfg.TunnelWait) * time.Millisecond)

	var result Result
	result.Resolver = resolver
	result.Download = "off"
	result.Whois = "off"
	result.Probe = "off"

	bestPriority := 0

	if cfg.Download {
		result.Download = "fail"
		latency, bytesRead, ok := doHTTPCheck(client, cfg.DownloadURL, cfg.DownloadTimeout, true)
		if ok {
			result.Download = "ok"
			result.LatencyMS = latency
			bestPriority = 3
			if latency > 0 {
				result.DownloadSpeedKBps = float64(bytesRead) / (float64(latency) / 1000.0) / 1024.0
			}
		}
	}

	if cfg.Whois {
		result.Whois = "fail"
		latency, org, country, ok := lookupResolverInfo(client, resolver, cfg.WhoisTimeout)
		if ok {
			result.Whois = "ok"
			result.Org = org
			result.Country = country
			if bestPriority < 2 {
				result.LatencyMS = latency
				bestPriority = 2
			}
		}
	}

	if cfg.Probe {
		result.Probe = "fail"
		latency, _, ok := doHTTPCheck(client, cfg.TestURL, cfg.Timeout, false)
		if ok {
			result.Probe = "ok"
			if bestPriority < 1 {
				result.LatencyMS = latency
				bestPriority = 1
			}
		}
	}

	if bestPriority == 0 {
		return false
	}

	printResult(cfg, result)

	// Save the working resolver IP to the output file.
	if outFile != nil {
		outMu.Lock()
		fmt.Fprintln(outFile, resolver)
		outMu.Unlock()
	}

	return true
}

func printResult(cfg *Config, result Result) {
	if cfg.JSON {
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return
	}

	fmt.Println(formatPlainTextResult(result, cfg.Colorize))
}

func formatLatency(latencyMs int64, colorize bool) string {
	latency := fmt.Sprintf("%dms", latencyMs)
	if !colorize {
		return latency
	}

	switch {
	case latencyMs <= 2000:
		return "\033[32m" + latency + "\033[0m"
	case latencyMs <= 6000:
		return "\033[33m" + latency + "\033[0m"
	default:
		return "\033[31m" + latency + "\033[0m"
	}
}

func formatPlainTextResult(result Result, colorize bool) string {
	line := fmt.Sprintf("%s %s", result.Resolver, formatLatency(result.LatencyMS, colorize))
	parts := []string{line}
	dl := "download=" + strconv.Quote(result.Download)
	if result.Download == "ok" && result.DownloadSpeedKBps > 0 {
		dl += fmt.Sprintf(" speed=%s", formatSpeed(result.DownloadSpeedKBps, colorize))
	}
	parts = append(parts, dl)
	parts = append(parts, "whois="+strconv.Quote(result.Whois))
	parts = append(parts, "probe="+strconv.Quote(result.Probe))
	if result.Org != "" {
		parts = append(parts, "org="+strconv.Quote(result.Org))
	}
	if result.Country != "" {
		parts = append(parts, "country="+strconv.Quote(result.Country))
	}
	return strings.Join(parts, " ")
}

func formatSpeed(kbps float64, colorize bool) string {
	var s string
	if kbps >= 1024 {
		s = fmt.Sprintf("%.1fMB/s", kbps/1024)
	} else {
		s = fmt.Sprintf("%.0fKB/s", kbps)
	}
	if !colorize {
		return s
	}
	switch {
	case kbps >= 512:
		return "\033[32m" + s + "\033[0m"
	case kbps >= 128:
		return "\033[33m" + s + "\033[0m"
	default:
		return "\033[31m" + s + "\033[0m"
	}
}

func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func doHTTPCheck(client *http.Client, targetURL string, timeoutSeconds int, drainBody bool) (latency int64, bytesRead int64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return 0, 0, false
	}
	req.Header.Set("Connection", "close")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()

	if drainBody {
		n, err := io.Copy(io.Discard, resp.Body)
		if err != nil {
			return 0, 0, false
		}
		bytesRead = n
	}

	return time.Since(start).Milliseconds(), bytesRead, true
}

func lookupResolverInfo(client *http.Client, resolver string, timeoutSeconds int) (int64, string, string, bool) {
	host, _, err := net.SplitHostPort(resolver)
	if err != nil {
		return 0, "unknown", "unknown", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", whoisURL+"/"+host, nil)
	if err != nil {
		return 0, "unknown", "unknown", false
	}
	req.Header.Set("Connection", "close")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, "unknown", "unknown", false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "unknown", "unknown", false
	}

	var data whoisResponse
	if err := json.Unmarshal(body, &data); err != nil || strings.TrimSpace(data.Status) != "ok" {
		return 0, "unknown", "unknown", false
	}

	org := strings.TrimSpace(data.OrgName)
	if org == "" {
		org = "unknown"
	}
	country := strings.TrimSpace(data.Country)
	if country == "" {
		country = "unknown"
	}
	return time.Since(start).Milliseconds(), org, country, true
}

func buildEngineArgs(cfg *Config, resolver string, port int) ([]string, error) {
	spec := engineSpecs[cfg.Engine]
	listenAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	extraArgs := cfg.ParsedArgs

	args := make([]string, 0, len(spec.DefaultArgs)+8)
	if spec.InsertArgsBeforeTail {
		tailSize := 2
		if len(spec.DefaultArgs) < tailSize {
			return nil, errors.New("invalid engine configuration")
		}
		args = append(args, spec.DefaultArgs[:len(spec.DefaultArgs)-tailSize]...)
		args = append(args, extraArgs...)
		args = append(args, spec.DefaultArgs[len(spec.DefaultArgs)-tailSize:]...)
	} else {
		args = append(args, spec.DefaultArgs...)
		args = append(args, extraArgs...)
	}
	return expandPlaceholders(args, placeholderValues(cfg, resolver, port, listenAddr))
}

func placeholderValues(cfg *Config, resolver string, port int, listenAddr string) map[string]string {
	return map[string]string{
		"{resolver}":    resolver,
		"{domain}":      cfg.Domain,
		"{listen}":      listenAddr,
		"{listen_host}": "127.0.0.1",
		"{listen_port}": strconv.Itoa(port),
	}
}

func expandPlaceholders(args []string, values map[string]string) ([]string, error) {
	expanded := make([]string, 0, len(args))
	for _, arg := range args {
		current := arg
		for key, value := range values {
			current = strings.ReplaceAll(current, key, value)
		}
		if strings.Contains(current, "{") && strings.Contains(current, "}") {
			return nil, fmt.Errorf("unknown placeholder in argument %q", arg)
		}
		expanded = append(expanded, current)
	}
	return expanded, nil
}

func splitCommandLine(input string) ([]string, error) {
	var args []string
	var current strings.Builder
	var quote rune
	escaped := false

	flush := func() {
		if current.Len() > 0 {
			args = append(args, current.String())
			current.Reset()
		}
	}

	for _, r := range input {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			current.WriteRune(r)
		}
	}

	if escaped {
		return nil, errors.New("unfinished escape sequence")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}

	flush()
	return args, nil
}
