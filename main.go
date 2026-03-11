package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Engine        string
	ClientPath    string
	Domain        string
	PubKey        string
	ResolversFile string
	TestURL       string
	Proxy         string
	ProxyUser     string
	ProxyPass     string
	Workers       int
	Retries       int
	TunnelWait    int
	Timeout       int
	StartPort     int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags()

	if err := validateConfig(cfg); err != nil {
		flag.Usage()
		return err
	}

	resolvers, err := loadResolvers(cfg.ResolversFile)
	if err != nil {
		return err
	}

	jobs := make(chan string, cfg.Workers*2)
	var wg sync.WaitGroup

	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)
		go func(port int) {
			defer wg.Done()
			worker(port, cfg, jobs)
		}(cfg.StartPort + i)
	}

	for _, r := range resolvers {
		jobs <- r
	}
	close(jobs)

	wg.Wait()
	return nil
}

func parseFlags() *Config {
	c := &Config{}

	flag.StringVar(&c.ResolversFile, "r", "", "Path to file containing resolvers (IP or IP:PORT per line)")
	flag.StringVar(&c.Engine, "e", "dnstt", "Tunnel engine to use: dnstt|slipstream")
	flag.StringVar(&c.ClientPath, "p", "", "Explicit path to client binary (optional)")
	flag.StringVar(&c.Domain, "d", "", "Tunnel domain (e.g., ns.example.com)")
	flag.StringVar(&c.PubKey, "k", "", "DNSTT public key (required if -e is dnstt)")
	flag.StringVar(&c.TestURL, "u", "http://www.google.com/gen_204", "HTTP URL to test through the tunnel")
	flag.StringVar(&c.Proxy, "x", "http", "Protocol to use when sending request through the tunnel: http|https|socks5|socks5h")
	flag.StringVar(&c.ProxyUser, "U", "", "Proxy username (if the tunnel exit requires auth)")
	flag.StringVar(&c.ProxyPass, "P", "", "Proxy password (if the tunnel exit requires auth)")
	flag.IntVar(&c.Workers, "w", 20, "Number of concurrent scanning workers")
	flag.IntVar(&c.Retries, "R", 0, "Number of retries per resolver after the first failure")
	flag.IntVar(&c.TunnelWait, "s", 1000, "Time to wait (ms) for tunnel establishment before testing HTTP")
	flag.IntVar(&c.Timeout, "t", 5, "HTTP request timeout in seconds")
	flag.IntVar(&c.StartPort, "l", 40000, "Starting local port for tunnel listeners")

	flag.Parse()

	c.Engine = strings.ToLower(strings.TrimSpace(c.Engine))
	c.Proxy = strings.ToLower(strings.TrimSpace(c.Proxy))
	return c
}

func validateConfig(cfg *Config) error {
	if cfg.ResolversFile == "" || cfg.Domain == "" {
		return errors.New("-r and -d are required")
	}

	switch cfg.Engine {
	case "dnstt":
		if cfg.PubKey == "" {
			return errors.New("-k is required for dnstt")
		}
	case "slipstream":
	default:
		return errors.New("-e must be one of: dnstt, slipstream")
	}

	switch cfg.Proxy {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("-x must be one of: http, https, socks5, socks5h")
	}

	if cfg.ProxyPass != "" && cfg.ProxyUser == "" {
		return errors.New("-P requires -U")
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
		path, err := exec.LookPath(cfg.Engine + "-client")
		if err != nil {
			return fmt.Errorf("binary %s-client not found in PATH; use -p to specify path", cfg.Engine)
		}
		cfg.ClientPath = path
	}
	return nil
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

func worker(port int, cfg *Config, jobs <-chan string) {
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
			if try(resolver, port, cfg, client) {
				break
			}
		}
	}
}

func try(resolver string, port int, cfg *Config, client *http.Client) bool {
	var cmd *exec.Cmd
	laddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	if cfg.Engine == "dnstt" {
		cmd = exec.Command(cfg.ClientPath,
			"-udp", resolver,
			"-pubkey", cfg.PubKey,
			cfg.Domain,
			laddr,
		)
	} else {
		cmd = exec.Command(cfg.ClientPath,
			"--tcp-listen-host", "127.0.0.1",
			"--tcp-listen-port", strconv.Itoa(port),
			"--resolver", resolver,
			"--domain", cfg.Domain,
			"--keep-alive-interval", "200",
		)
	}

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

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Timeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", cfg.TestURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Connection", "close")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	fmt.Printf("%s %dms\n", resolver, time.Since(start).Milliseconds())
	return true
}
