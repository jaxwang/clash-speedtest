package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"encoding/json"
	"math/rand"
	"errors"

	"github.com/Dreamacro/clash/adapter"
	"github.com/Dreamacro/clash/adapter/provider"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"
	"gopkg.in/yaml.v3"
)

var (
	livenessObject     = flag.String("l", "https://speed.cloudflare.com/__down?bytes=%d", "liveness object, support http(s) url, support payload too")
	configPathConfig   = flag.String("c", "", "configuration file path, also support http(s) url")
	filterRegexConfig  = flag.String("f", ".*", "filter proxies by name, use regexp")
	downloadSizeConfig = flag.Int("size", 1024*1024*100, "download size for testing proxies")
	timeoutConfig      = flag.Duration("timeout", time.Second*5, "timeout for testing proxies")
	sortField          = flag.String("sort", "b", "sort field for testing proxies, b for bandwidth, t for TTFB")
	output             = flag.String("output", "", "output result to csv/yaml file")
	concurrent         = flag.Int("concurrent", 4, "download concurrent size")
	ipTokenList 	   = flag.String("iptokens", "", "comma-separated list of ipinfo.io tokens")

)

type CProxy struct {
	C.Proxy
	SecretConfig any
}

type Result struct {
	Name      string
	Bandwidth float64
	TTFB      time.Duration
	CountryCode	string
	IP		  string
}

var (
	red   = "\033[31m"
	green = "\033[32m"
)

type RawConfig struct {
	Providers map[string]map[string]any `yaml:"proxy-providers"`
	Proxies   []map[string]any          `yaml:"proxies"`
}

func main() {
	flag.Parse()

	// 解析并分割字符串
	ipTokenArray := strings.Split(*ipTokenList, ",")

	// 检查是否以逗号结尾，判断是否有空字符串
	if strings.HasSuffix(*ipTokenList, ",") {
		ipTokenArray = append(ipTokenArray, "")
	}

	C.UA = "clash.meta"

	if *configPathConfig == "" {
		log.Fatalln("Please specify the configuration file")
	}

	var allProxies = make(map[string]CProxy)
	for _, configPath := range strings.Split(*configPathConfig, ",") {
		var body []byte
		var err error
		if strings.HasPrefix(configPath, "http") {
			var resp *http.Response
			resp, err = http.Get(configPath)
			if err != nil {
				log.Warnln("failed to fetch config: %s", err)
				continue
			}
			body, err = io.ReadAll(resp.Body)
		} else {
			body, err = os.ReadFile(configPath)
		}
		if err != nil {
			log.Warnln("failed to read config: %s", err)
			continue
		}

		lps, err := loadProxies(body)
		if err != nil {
			log.Fatalln("Failed to convert : %s", err)
		}

		for k, p := range lps {
			if _, ok := allProxies[k]; !ok {
				allProxies[k] = p
			}
		}
	}

	filteredProxies := filterProxies(*filterRegexConfig, allProxies)
	results := make([]Result, 0, len(filteredProxies))

	//format := "%s%-42s\t%-12s\t%-12s\033[0m\n"
	format := "%s%-42s\t%-12s\t%-12s\t%-9s\t%-15s\033[0m\n"

	fmt.Printf(format, "", "节点", "带宽", "延迟", "国家代码", "IP")
	for _, name := range filteredProxies {
		proxy := allProxies[name]
		switch proxy.Type() {
		case C.Shadowsocks, C.ShadowsocksR, C.Snell, C.Socks5, C.Http, C.Vmess, C.Vless, C.Trojan, C.Hysteria, C.Hysteria2, C.WireGuard, C.Tuic:
			result := TestProxyConcurrent(name, proxy, *downloadSizeConfig, *timeoutConfig, *concurrent , ipTokenArray)
			result.Printf(format)
			results = append(results, *result)
		case C.Direct, C.Reject, C.Relay, C.Selector, C.Fallback, C.URLTest, C.LoadBalance:
			continue
		default:
			log.Fatalln("Unsupported proxy type: %s", proxy.Type())
		}
	}

	if *sortField != "" {
		switch *sortField {
		case "b", "bandwidth":
			sort.Slice(results, func(i, j int) bool {
				return results[i].Bandwidth > results[j].Bandwidth
			})
			fmt.Println("\n\n===结果按照带宽排序===")
		case "t", "ttfb":
			sort.Slice(results, func(i, j int) bool {
				return results[i].TTFB < results[j].TTFB
			})
			fmt.Println("\n\n===结果按照延迟排序===")
		default:
			log.Fatalln("Unsupported sort field: %s", *sortField)
		}
		fmt.Printf(format, "", "节点", "带宽", "延迟", "国家代码", "IP")
		for _, result := range results {
			result.Printf(format)
		}
	}

	if strings.EqualFold(*output, "yaml") {
		if err := writeNodeConfigurationToYAML("result.yaml", results, allProxies); err != nil {
			log.Fatalln("Failed to write yaml: %s", err)
		}
	} else if strings.EqualFold(*output, "csv") {
		if err := writeToCSV("result.csv", results); err != nil {
			log.Fatalln("Failed to write csv: %s", err)
		}
	}
}

func filterProxies(filter string, proxies map[string]CProxy) []string {
	filterRegexp := regexp.MustCompile(filter)
	filteredProxies := make([]string, 0, len(proxies))
	for name := range proxies {
		if filterRegexp.MatchString(name) {
			filteredProxies = append(filteredProxies, name)
		}
	}
	sort.Strings(filteredProxies)
	return filteredProxies
}

func loadProxies(buf []byte) (map[string]CProxy, error) {
	rawCfg := &RawConfig{
		Proxies: []map[string]any{},
	}
	if err := yaml.Unmarshal(buf, rawCfg); err != nil {
		return nil, err
	}
	proxies := make(map[string]CProxy)
	proxiesConfig := rawCfg.Proxies
	providersConfig := rawCfg.Providers

	for i, config := range proxiesConfig {
		proxy, err := adapter.ParseProxy(config)
		if err != nil {
			return nil, fmt.Errorf("proxy %d: %w", i, err)
		}

		if _, exist := proxies[proxy.Name()]; exist {
			return nil, fmt.Errorf("proxy %s is the duplicate name", proxy.Name())
		}
		proxies[proxy.Name()] = CProxy{Proxy: proxy, SecretConfig: config}
	}
	for name, config := range providersConfig {
		if name == provider.ReservedName {
			return nil, fmt.Errorf("can not defined a provider called `%s`", provider.ReservedName)
		}
		pd, err := provider.ParseProxyProvider(name, config)
		if err != nil {
			return nil, fmt.Errorf("parse proxy provider %s error: %w", name, err)
		}
		if err := pd.Initial(); err != nil {
			return nil, fmt.Errorf("initial proxy provider %s error: %w", pd.Name(), err)
		}
		for _, proxy := range pd.Proxies() {
			proxies[fmt.Sprintf("[%s] %s", name, proxy.Name())] = CProxy{Proxy: proxy}
		}
	}
	return proxies, nil
}

func (r *Result) Printf(format string) {
	color := ""
	if r.Bandwidth < 1024*1024 {
		color = red
	} else if r.Bandwidth > 1024*1024*10 {
		color = green
	}
	fmt.Printf(format, color, formatName(r.Name), formatBandwidth(r.Bandwidth), formatMilliseconds(r.TTFB), r.CountryCode,r.IP)
}

// 返回随机 User-Agent 的函数 有些获取ip的api需要设置UA
func getRandomUserAgent() string {
    userAgents := []string{
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.1 Safari/605.1.15",
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/116.0.0.0 Safari/537.36",
        "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1",
        "Mozilla/5.0 (iPad; CPU OS 15_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.0 Mobile/15E148 Safari/604.1",
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:105.0) Gecko/20100101 Firefox/105.0",
        "Mozilla/5.0 (Linux; Android 13; Pixel 6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Mobile Safari/537.36",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 13_0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.5938.88 Safari/537.36",
        "Mozilla/5.0 (Windows NT 6.1; WOW64; rv:115.0) Gecko/20100101 Firefox/115.0",
        "Mozilla/5.0 (X11; Linux i686; rv:91.0) Gecko/20100101 Firefox/91.0",
        "Mozilla/5.0 (Linux; Android 10; SM-G973U) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/109.0.0.0 Mobile Safari/537.36",
        "Mozilla/5.0 (Macintosh; PPC Mac OS X 10_6_8) AppleWebKit/534.30 (KHTML, like Gecko) Version/5.1 Safari/534.30",
        "Mozilla/5.0 (Windows NT 6.3; ARM; Trident/7.0; Touch; rv:11.0) like Gecko",
        "Mozilla/5.0 (X11; Linux i686; rv:68.0) Gecko/20100101 Firefox/68.0",
        "Mozilla/5.0 (Linux; U; Android 9; en-US; SM-J810Y Build/PPR1.180610.011) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Mobile Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    }

    rand.Seed(time.Now().UnixNano()) // 设置随机数种子
    return userAgents[rand.Intn(len(userAgents))]
}

func checkCountry(result map[string]interface{}, ip_url string) (string, string, bool) {

	if ip, ok := result["ip"].(string); ok && ip != "" {
		// 优先尝试获取 country_code 字段
		if country, ok := result["country_code"].(string); ok && country != "" {
			return country, ip, true

		}else if country, ok := result["country"].(string); ok && country != "" { // 再次尝试获取 country 字段
			return country, ip, true
		}else{
			return "", ip, true
		}
	}else{
		return "", "", false
	}
}

func queryIPLocation(name string, proxy C.Proxy,timeout time.Duration,ipTokenArray []string) (string , string , error){

	client := http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var u16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err == nil {
					u16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &C.Metadata{
					Host:    host,
					DstPort: u16Port,
				})
			},
		},
	}

	//定义api切片
	apiURLs := []string{}

	rand.Seed(time.Now().UnixNano())
	//randomIndex := rand.Intn(len(ipTokenArray))

	for _, token := range ipTokenArray {
        apiURLs = append(apiURLs,fmt.Sprintf("http://ipinfo.io/json?token=%s", token))
    }

	// 随机打乱数组 , 做api的负载均衡
	rand.Shuffle(len(apiURLs), func(i, j int) {
		apiURLs[i], apiURLs[j] = apiURLs[j], apiURLs[i]
	})
	
	// 用ip.sb做最后的兜底方案
	apiURLs = append(apiURLs, "https://api.ip.sb/geoip")

	// 依次尝试每个 API
	for _, ip_url := range apiURLs {

		var result map[string]interface{}
		// 创建请求
		req, err := http.NewRequest("GET", ip_url, nil)
		if err != nil {
			fmt.Println("Error creating request:", err)
			continue
		}

		// 设置 User-Agent
		req.Header.Set("User-Agent", getRandomUserAgent())

		// 执行请求
		resp, err := client.Do(req)
		if err != nil {
			//fmt.Printf("Error requesting %s: %v\n", ip_url, err)
			continue
		}

		defer resp.Body.Close()

		// 检查状态码是否为 200
		if resp.StatusCode != http.StatusOK {
			//fmt.Printf("Failed to get data from %s: %s\n", ip_url, resp.Status)
			continue
		}

		// 尝试解析返回的 JSON
		err = json.NewDecoder(resp.Body).Decode(&result)
		if err != nil {
			//fmt.Printf("Error decoding JSON from %s: %v\n", ip_url, err)
			continue
		}

		if country_code, ip, ok := checkCountry(result, ip_url); ok {
			//fmt.Println(ip_url)
			//fmt.Printf("%+v\n", result)
			return country_code , ip, nil
		} else {
			//fmt.Printf("Invalid data from %s: %+v\n", ip_url, result)
			continue
		}
	}
	// 如果所有 API 都失败，返回错误
	return "", "", errors.New("all APIs failed or returned invalid data")
}


func TestProxyConcurrent(name string, proxy C.Proxy, downloadSize int, timeout time.Duration, concurrentCount int, ipTokenArray []string) *Result {
	if concurrentCount <= 0 {
		concurrentCount = 1
	}

	chunkSize := downloadSize / concurrentCount
	totalTTFB := int64(0)
	downloaded := int64(0)

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < concurrentCount; i++ {
		wg.Add(1)
		go func(i int) {
			result, w := TestProxy(name, proxy, chunkSize, timeout)
			if w != 0 {
				atomic.AddInt64(&downloaded, w)
				atomic.AddInt64(&totalTTFB, int64(result.TTFB))
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	downloadTime := time.Since(start)

	result := &Result{
		Name:      name,
		Bandwidth: float64(downloaded) / downloadTime.Seconds(),
		TTFB:      time.Duration(totalTTFB / int64(concurrentCount)),
		CountryCode: "NIL", //新增
		IP:			"NIL", //新增
	}

	// 添加获取country_code 逻辑
	const epsilon = 1e-9 // 一个很小的值
	if result.Bandwidth > epsilon {
		country_code, ip, err := queryIPLocation(name, proxy, timeout*2, ipTokenArray)
		if err == nil {
			if country_code != "" {
				result.CountryCode = country_code
			}
			result.IP = ip
		}
	}

	return result
}

func TestProxy(name string, proxy C.Proxy, downloadSize int, timeout time.Duration) (*Result, int64) {
	client := http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var u16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err == nil {
					u16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &C.Metadata{
					Host:    host,
					DstPort: u16Port,
				})
			},
		},
	}

	start := time.Now()
	resp, err := client.Get(fmt.Sprintf(*livenessObject, downloadSize))
	if err != nil {
		return &Result{name, -1, -1, "", ""}, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode-http.StatusOK > 100 {
		return &Result{name, -1, -1, "", ""}, 0
	}
	ttfb := time.Since(start)

	written, _ := io.Copy(io.Discard, resp.Body)
	if written == 0 {
		return &Result{name, -1, -1, "", ""}, 0
	}

	downloadTime := time.Since(start) - ttfb
	bandwidth := float64(written) / downloadTime.Seconds()

	return &Result{name, bandwidth, ttfb, "", ""}, written
}

var (
	emojiRegex = regexp.MustCompile(`[\x{1F600}-\x{1F64F}\x{1F300}-\x{1F5FF}\x{1F680}-\x{1F6FF}\x{2600}-\x{26FF}\x{1F1E0}-\x{1F1FF}]`)
	spaceRegex = regexp.MustCompile(`\s{2,}`)
)

func formatName(name string) string {
	noEmoji := emojiRegex.ReplaceAllString(name, "")
	mergedSpaces := spaceRegex.ReplaceAllString(noEmoji, " ")
	return strings.TrimSpace(mergedSpaces)
}

func formatBandwidth(v float64) string {
	if v <= 0 {
		return "N/A"
	}
	if v < 1024 {
		return fmt.Sprintf("%.02fB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fKB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fMB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fGB/s", v)
	}
	v /= 1024
	return fmt.Sprintf("%.02fTB/s", v)
}

func formatMilliseconds(v time.Duration) string {
	if v <= 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.02fms", float64(v.Milliseconds()))
}

func writeNodeConfigurationToYAML(filePath string, results []Result, proxies map[string]CProxy) error {
	fp, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer fp.Close()

	var sortedProxies []any
	for _, result := range results {
		if v, ok := proxies[result.Name]; ok {
			sortedProxies = append(sortedProxies, v.SecretConfig)
		}
	}

	bytes, err := yaml.Marshal(sortedProxies)
	if err != nil {
		return err
	}

	_, err = fp.Write(bytes)
	return err
}

func writeToCSV(filePath string, results []Result) error {
	csvFile, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer csvFile.Close()

	// 写入 UTF-8 BOM 头
	csvFile.WriteString("\xEF\xBB\xBF")

	csvWriter := csv.NewWriter(csvFile)
	err = csvWriter.Write([]string{"节点", "带宽 (MB/s)", "延迟 (ms)", "国家代码", "IP"})
	if err != nil {
		return err
	}
	for _, result := range results {
		line := []string{
			result.Name,
			fmt.Sprintf("%.2f", result.Bandwidth/1024/1024),
			strconv.FormatInt(result.TTFB.Milliseconds(), 10),
			result.CountryCode,
			result.IP,
		}
		err = csvWriter.Write(line)
		if err != nil {
			return err
		}
	}
	csvWriter.Flush()
	return nil
}
