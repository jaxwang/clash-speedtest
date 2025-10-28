package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"
	"strings"
	"net"
	"net/http"
	"context"
	"strconv"
	"encoding/json"
	"errors"
	"math/rand"

	"github.com/faceair/clash-speedtest/speedtester"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/constant"
	"github.com/olekukonko/tablewriter"
	"github.com/schollz/progressbar/v3"
	"gopkg.in/yaml.v3"
)

var (
	configPathsConfig = flag.String("c", "", "config file path, also support http(s) url")
	filterRegexConfig = flag.String("f", ".+", "filter proxies by name, use regexp")
	serverURL         = flag.String("server-url", "https://speed.cloudflare.com", "server url")
	downloadSize      = flag.Int("download-size", 50*1024*1024, "download size for testing proxies")
	uploadSize        = flag.Int("upload-size", 20*1024*1024, "upload size for testing proxies")
	timeout           = flag.Duration("timeout", time.Second*5, "timeout for testing proxies")
	concurrent        = flag.Int("concurrent", 4, "download concurrent size")
	outputPath        = flag.String("output", "", "output config file path")
	maxLatency        = flag.Duration("max-latency", 800*time.Millisecond, "filter latency greater than this value")
	minSpeed          = flag.Float64("min-speed", 5, "filter speed less than this value(unit: MB/s)")
	ipTokenList       = flag.String("iptokens", "", "comma-separated list of ipinfo.io tokens")
)

const (
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorReset  = "\033[0m"
)

// ExtendedResult 扩展的结果结构，包含国家代码和IP信息
type ExtendedResult struct {
	speedtester.Result
	CountryCode string
	IP          string
}

func main() {
	flag.Parse()
	log.SetLevel(log.SILENT)

	if *configPathsConfig == "" {
		log.Fatalln("please specify the configuration file")
	}

	speedTester := speedtester.New(&speedtester.Config{
		ConfigPaths:  *configPathsConfig,
		FilterRegex:  *filterRegexConfig,
		ServerURL:    *serverURL,
		DownloadSize: *downloadSize,
		UploadSize:   *uploadSize,
		Timeout:      *timeout,
		Concurrent:   *concurrent,
	})

	allProxies, err := speedTester.LoadProxies()
	if err != nil {
		log.Fatalln("load proxies failed: %v", err)
	}

	// 解析并分割字符串
	ipTokenArray := strings.Split(*ipTokenList, ",")

	// 检查是否以逗号结尾，判断是否有空字符串
	if strings.HasSuffix(*ipTokenList, ",") {
		ipTokenArray = append(ipTokenArray, "")
	}

	bar := progressbar.Default(int64(len(allProxies)), "测试中...")
	results := make([]*ExtendedResult, 0)
	
	// 使用 speedtester 的 TestProxies 方法进行测试
	speedTester.TestProxies(allProxies, func(result *speedtester.Result) {
		extendedResult := &ExtendedResult{
			Result: *result,
		}
		
		// 添加获取country_code和IP的逻辑
		const epsilon = 1e-9 // 一个很小的值
		if result.DownloadSpeed > epsilon {
			proxy := allProxies[result.ProxyName]
			if proxy != nil {
				countryCode, ip, err := queryIPLocation(result.ProxyName, proxy.Proxy, *timeout*2, ipTokenArray)
				if err == nil {
					extendedResult.CountryCode = countryCode
					extendedResult.IP = ip
				}
			}
		}
		
		bar.Add(1)
		bar.Describe(result.ProxyName)
		results = append(results, extendedResult)
	})

	sort.Slice(results, func(i, j int) bool {
		return results[i].DownloadSpeed > results[j].DownloadSpeed
	})

	printResults(results)

	if *outputPath != "" {
		err = saveConfig(results)
		if err != nil {
			log.Fatalln("save config file failed: %v", err)
		}
		fmt.Printf("\nsave config file to: %s\n", *outputPath)
	}
}

func printResults(results []*ExtendedResult) {
	table := tablewriter.NewWriter(os.Stdout)

	table.SetHeader([]string{
		"序号",
		"节点名称",
		"类型",
		"延迟",
		"抖动",
		"丢包率",
		"下载速度",
		"上传速度",
		"国家代码",
		"IP",
	})

	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(true)
	table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("")
	table.SetColumnSeparator("")
	table.SetRowSeparator("")
	table.SetHeaderLine(false)
	table.SetBorder(false)
	table.SetTablePadding("\t")
	table.SetNoWhiteSpace(true)

	for i, result := range results {
		idStr := fmt.Sprintf("%d.", i+1)

		// 延迟颜色
		latencyStr := result.FormatLatency()
		if result.Latency > 0 {
			if result.Latency < 800*time.Millisecond {
				latencyStr = colorGreen + latencyStr + colorReset
			} else if result.Latency < 1500*time.Millisecond {
				latencyStr = colorYellow + latencyStr + colorReset
			} else {
				latencyStr = colorRed + latencyStr + colorReset
			}
		} else {
			latencyStr = colorRed + latencyStr + colorReset
		}

		jitterStr := result.FormatJitter()
		if result.Jitter > 0 {
			if result.Jitter < 800*time.Millisecond {
				jitterStr = colorGreen + jitterStr + colorReset
			} else if result.Jitter < 1500*time.Millisecond {
				jitterStr = colorYellow + jitterStr + colorReset
			} else {
				jitterStr = colorRed + jitterStr + colorReset
			}
		} else {
			jitterStr = colorRed + jitterStr + colorReset
		}

		// 丢包率颜色
		packetLossStr := result.FormatPacketLoss()
		if result.PacketLoss < 10 {
			packetLossStr = colorGreen + packetLossStr + colorReset
		} else if result.PacketLoss < 20 {
			packetLossStr = colorYellow + packetLossStr + colorReset
		} else {
			packetLossStr = colorRed + packetLossStr + colorReset
		}

		// 下载速度颜色 (以MB/s为单位判断)
		downloadSpeed := result.DownloadSpeed / (1024 * 1024)
		downloadSpeedStr := result.FormatDownloadSpeed()
		if downloadSpeed >= 10 {
			downloadSpeedStr = colorGreen + downloadSpeedStr + colorReset
		} else if downloadSpeed >= 5 {
			downloadSpeedStr = colorYellow + downloadSpeedStr + colorReset
		} else {
			downloadSpeedStr = colorRed + downloadSpeedStr + colorReset
		}

		// 上传速度颜色
		uploadSpeed := result.UploadSpeed / (1024 * 1024)
		uploadSpeedStr := result.FormatUploadSpeed()
		if uploadSpeed >= 5 {
			uploadSpeedStr = colorGreen + uploadSpeedStr + colorReset
		} else if uploadSpeed >= 2 {
			uploadSpeedStr = colorYellow + uploadSpeedStr + colorReset
		} else {
			uploadSpeedStr = colorRed + uploadSpeedStr + colorReset
		}

		row := []string{
			idStr,
			result.ProxyName,
			result.ProxyType,
			latencyStr,
			jitterStr,
			packetLossStr,
			downloadSpeedStr,
			uploadSpeedStr,
			result.CountryCode,
			result.IP,
		}

		table.Append(row)
	}

	fmt.Println()
	table.Render()
	fmt.Println()
}

func saveConfig(results []*ExtendedResult) error {
	filteredResults := make([]*ExtendedResult, 0)
	for _, result := range results {
		if *maxLatency > 0 && result.Latency > *maxLatency {
			continue
		}
		if *minSpeed > 0 && float64(result.DownloadSpeed)/(1024*1024) < *minSpeed {
			continue
		}
		filteredResults = append(filteredResults, result)
	}

	proxies := make([]map[string]any, 0)
	for _, result := range filteredResults {
		proxies = append(proxies, result.ProxyConfig)
	}

	config := &speedtester.RawConfig{
		Proxies: proxies,
	}
	
	yamlData, err := yaml.Marshal(config)
	if err != nil {
		return err
	}

	return os.WriteFile(*outputPath, yamlData, 0o644)
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
		} else if country, ok := result["country"].(string); ok && country != "" { // 再次尝试获取 country 字段
			return country, ip, true
		} else {
			return "", ip, true
		}
	} else {
		return "", "", false
	}
}

func queryIPLocation(name string, proxy constant.Proxy, timeout time.Duration, ipTokenArray []string) (string, string, error) {
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
				return proxy.DialContext(ctx, &constant.Metadata{
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
        apiURLs = append(apiURLs, fmt.Sprintf("http://ipinfo.io/json?token=%s", token))
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
			return country_code, ip, nil
		} else {
			//fmt.Printf("Invalid data from %s: %+v\n", ip_url, result)
			continue
		}
	}
	// 如果所有 API 都失败，返回错误
	return "", "", errors.New("all APIs failed or returned invalid data")
}