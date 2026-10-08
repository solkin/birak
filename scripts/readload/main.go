// Command readload drives disposable TLS/SigV4 APK and icon acceptance traffic.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type object struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}
type fixture struct {
	Objects map[string]object `json:"objects"`
}
type series struct {
	P50     float64 `json:"p50"`
	P99     float64 `json:"p99"`
	Worst   float64 `json:"worst"`
	Samples int     `json:"samples"`
}
type result struct {
	IconGETs        int64             `json:"icon_gets"`
	Icon304         int64             `json:"icon_304"`
	APKGETs         int64             `json:"apk_gets"`
	IconBytes       int64             `json:"icon_bytes"`
	APKBytes        int64             `json:"apk_bytes"`
	Elapsed         float64           `json:"elapsed_seconds"`
	MiBPerSecond    float64           `json:"mib_per_second"`
	APKMiBPerSecond float64           `json:"apk_mib_per_second"`
	APKTBPerDay     float64           `json:"apk_tb_per_day"`
	IconRPS         float64           `json:"icon_requests_per_second"`
	Latencies       map[string]series `json:"latencies"`
	Errors          []string          `json:"errors"`
	TLS             string            `json:"tls"`
}

func digest(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}
func sign(req *http.Request) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	day := stamp[:8]
	empty := sha256.Sum256(nil)
	payload := hex.EncodeToString(empty[:])
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonical := req.Method + "\n" + req.URL.EscapedPath() + "\n" + req.URL.Query().Encode() + "\nhost:" + req.URL.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + stamp + "\n\n" + signed + "\n" + payload
	sum := sha256.Sum256([]byte(canonical))
	scope := day + "/us-east-1/s3/aws4_request"
	key := []byte("AWS4docker-acceptance-only")
	for _, part := range []string{day, "us-east-1", "s3", "aws4_request"} {
		key = digest(key, part)
	}
	signature := hex.EncodeToString(digest(key, "AWS4-HMAC-SHA256\n"+stamp+"\n"+scope+"\n"+hex.EncodeToString(sum[:])))
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/"+scope+", SignedHeaders="+signed+", Signature="+signature)
}
func stats(values []float64) series {
	if len(values) == 0 {
		return series{}
	}
	slices.Sort(values)
	return series{values[len(values)/2], values[min(len(values)-1, int(float64(len(values))*.99))], values[len(values)-1], len(values)}
}

type paced struct {
	io.Reader
	started time.Time
	bytes   int64
	rate    float64
}

func (p *paced) Read(b []byte) (int, error) {
	n, err := p.Reader.Read(b)
	p.bytes += int64(n)
	if delay := time.Duration(float64(p.bytes)/p.rate*float64(time.Second)) - time.Since(p.started); delay > 0 {
		time.Sleep(delay)
	}
	return n, err
}

func run() error {
	endpoint := flag.String("endpoint", "", "HTTPS S3 URL")
	serverName := flag.String("tls-server-name", "", "expected TLS identity when testing the internal Docker address")
	cert := flag.String("cert", "", "ephemeral test CA PEM")
	data := flag.String("fixture", "", "fixture JSON")
	duration := flag.Duration("duration", time.Minute, "stop starting requests after this interval; drain existing reads")
	icons := flag.Int("icon-readers", 24, "icon concurrency")
	apks := flag.Int("apk-readers", 8, "APK concurrency")
	apkRate := flag.Float64("apk-mib-per-second", 0, "aggregate offered APK consumption budget; 0 = unbounded")
	iconRate := flag.Int("icon-rps", 1000, "offered icon requests per second; 0 = unbounded")
	flag.Parse()
	if *icons < 1 || *apks < 1 || *duration <= 0 || *iconRate < 0 || *apkRate < 0 {
		return fmt.Errorf("invalid workload parameters")
	}
	f, err := os.Open(*data)
	if err != nil {
		return err
	}
	var input fixture
	err = json.NewDecoder(f).Decode(&input)
	f.Close()
	if err != nil {
		return err
	}
	var small, large []object
	for _, o := range input.Objects {
		if strings.Contains(o.Name, "/icons/") {
			small = append(small, o)
		} else if strings.Contains(o.Name, "/apks/") {
			large = append(large, o)
		}
	}
	slices.SortFunc(small, func(a, b object) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(large, func(a, b object) int { return strings.Compare(a.Name, b.Name) })
	if len(small) == 0 || len(large) == 0 {
		return fmt.Errorf("empty read fixture")
	}
	pem, err := os.ReadFile(*cert)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return fmt.Errorf("invalid test CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: *serverName, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}, MaxIdleConns: 128, MaxIdleConnsPerHost: *icons + *apks + 8, ResponseHeaderTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	var guard sync.Mutex
	var wg sync.WaitGroup
	out := result{Errors: []string{}, TLS: "verified TLS 1.2+"}
	times := map[string][]float64{"icon_ttfb": {}, "icon_seconds": {}, "apk_ttfb": {}, "apk_seconds": {}}
	begin := time.Now()
	until := begin.Add(*duration)
	worker := func(number int, big bool) {
		defer wg.Done()
		rng := rand.New(rand.NewSource(int64(number)))
		tags := map[string]string{}
		pool, prefix := small, "icon"
		if big {
			pool, prefix = large, "apk"
		}
		buf := make([]byte, 256<<10)
		var ticker *time.Ticker
		if !big && *iconRate > 0 {
			ticker = time.NewTicker(time.Duration(max(int64(time.Microsecond), int64(time.Second)*int64(*icons)/int64(*iconRate))))
			defer ticker.Stop()
		}
		fail := func(err error) { guard.Lock(); out.Errors = append(out.Errors, err.Error()); guard.Unlock() }
		for time.Now().Before(until) {
			if ticker != nil {
				<-ticker.C
				if !time.Now().Before(until) {
					break
				}
			}
			item := pool[rng.Intn(len(pool))]
			target, err := url.Parse(*endpoint + "/" + item.Name)
			if err != nil {
				fail(err)
				return
			}
			req, err := http.NewRequest("GET", target.String(), nil)
			if err != nil {
				fail(err)
				return
			}
			sign(req)
			if !big && tags[item.Name] != "" && rng.Intn(4) == 0 {
				req.Header.Set("If-None-Match", tags[item.Name])
			}
			start := time.Now()
			resp, err := client.Do(req)
			if err != nil {
				fail(err)
				return
			}
			ttfb := time.Since(start).Seconds()
			h := sha256.New()
			var body io.Reader = resp.Body
			if big && *apkRate > 0 {
				body = &paced{Reader: resp.Body, started: time.Now(), rate: *apkRate * (1 << 20) / float64(*apks)}
			}
			n, readErr := io.CopyBuffer(h, body, buf)
			closeErr := resp.Body.Close()
			if readErr != nil || closeErr != nil {
				fail(fmt.Errorf("read %s: %v %v", item.Name, readErr, closeErr))
				return
			}
			good := resp.StatusCode == 200 && n == item.Size && hex.EncodeToString(h.Sum(nil)) == item.Hash
			conditional := !big && resp.StatusCode == 304 && n == 0 && tags[item.Name] != "" && resp.Header.Get("ETag") == tags[item.Name]
			if !good && !conditional {
				fail(fmt.Errorf("invalid response %s: status=%d bytes=%d", item.Name, resp.StatusCode, n))
				return
			}
			tags[item.Name] = resp.Header.Get("ETag")
			guard.Lock()
			if big {
				out.APKGETs++
				out.APKBytes += n
			} else {
				out.IconGETs++
				out.IconBytes += n
				if conditional {
					out.Icon304++
				}
			}
			if len(times[prefix+"_ttfb"]) < 100000 {
				times[prefix+"_ttfb"] = append(times[prefix+"_ttfb"], ttfb)
				times[prefix+"_seconds"] = append(times[prefix+"_seconds"], time.Since(start).Seconds())
			}
			guard.Unlock()
		}
	}
	for i := 0; i < *icons+*apks; i++ {
		wg.Add(1)
		go worker(i, i >= *icons)
	}
	wg.Wait()
	out.Elapsed = time.Since(begin).Seconds()
	out.MiBPerSecond = float64(out.APKBytes+out.IconBytes) / out.Elapsed / (1 << 20)
	out.APKMiBPerSecond = float64(out.APKBytes) / out.Elapsed / (1 << 20)
	out.APKTBPerDay = float64(out.APKBytes) / out.Elapsed * 86400 / 1e12
	out.IconRPS = float64(out.IconGETs) / out.Elapsed
	out.Latencies = map[string]series{}
	for k, v := range times {
		out.Latencies[k] = stats(v)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("%d read failures", len(out.Errors))
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
