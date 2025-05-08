package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v3"
)

// AddressingStyle 表示S3寻址方式
type AddressingStyle string

const (
	// PathStyle 表示路径风格寻址 (如 https://s3.amazonaws.com/BUCKET/KEY)
	PathStyle AddressingStyle = "path"
	// VirtualHostedStyle 表示虚拟主机风格寻址 (如 https://BUCKET.s3.amazonaws.com/KEY)
	VirtualHostedStyle AddressingStyle = "virtual"
)

// EndpointConfig represents the configuration for a specific endpoint
type EndpointConfig struct {
	AccessKey       string          `json:"access_key" yaml:"access_key"`
	SecretKey       string          `json:"secret_key" yaml:"secret_key"`
	Endpoint        string          `json:"endpoint" yaml:"endpoint"`
	Region          string          `json:"region" yaml:"region"`
	PartSize        int64           `json:"part_size" yaml:"part_size"`
	Concurrency     int             `json:"concurrency" yaml:"concurrency"`
	ResumeUpload    bool            `json:"resume" yaml:"resume"` // 是否启用断点续传
	Proxy           string          `json:"proxy" yaml:"proxy"`
	Bucket          string          `json:"bucket" yaml:"bucket"`
	AddressingStyle AddressingStyle `json:"addressing_style" yaml:"addressing_style"`
	Insecure        bool            `json:"insecure" yaml:"insecure"` // 是否忽略TLS验证
}

// GlobalConfig represents global configuration options
type GlobalConfig struct {
	JSONOutput      bool            `json:"json_output" yaml:"json_output"`
	PartSize        int64           `json:"part_size" yaml:"part_size"`
	Concurrency     int             `json:"concurrency" yaml:"concurrency"`
	Proxy           string          `json:"proxy" yaml:"proxy"`
	Resume          bool            `json:"resume" yaml:"resume"`
	Region          string          `json:"region" yaml:"region"`
	StateDir        string          `json:"state_dir" yaml:"state_dir"` // 存储上传状态的目录
	AddressingStyle AddressingStyle `json:"addressing_style" yaml:"addressing_style"`
	Insecure        bool            `json:"insecure" yaml:"insecure"` // 是否忽略TLS验证
}

// ConfigFile represents the configuration file structure
type ConfigFile struct {
	Default  string                    `json:"default" yaml:"default"`
	Global   GlobalConfig              `json:"global" yaml:"global"`
	Profiles map[string]EndpointConfig `json:"-" yaml:"-"` // 使用动态键名存储配置
}

// Config represents the configuration for the application
type Config struct {
	AccessKey       string
	SecretKey       string
	Endpoint        string
	Region          string
	Bucket          string
	PartSize        int64
	Concurrency     int
	Proxy           string
	ResumeUpload    bool
	JSONOutput      bool
	StateDir        string
	AddressingStyle AddressingStyle
	Insecure        bool // 是否忽略TLS验证
}

// UploadState 存储上传状态信息
type UploadState struct {
	FilePath       string                `json:"file_path"`
	Key            string                `json:"key"`
	UploadID       string                `json:"upload_id"`
	FileSize       int64                 `json:"file_size"`
	PartSize       int64                 `json:"part_size"`
	CompletedParts []types.CompletedPart `json:"completed_parts"`
	LastModified   time.Time             `json:"last_modified"`
}

// ProgressReporter reports upload progress
type ProgressReporter struct {
	mu             sync.Mutex
	totalBytes     int64
	currentBytes   int64
	startTime      time.Time
	lastUpdateTime time.Time
	partsCompleted map[int]bool
	instantSpeed   float64
	avgSpeed       float64
	// 用于计算瞬时速度的滑动窗口
	speedWindow []struct {
		timestamp time.Time
		bytes     int64
	}
	// 每个分片的进度跟踪
	partProgress map[int]int64
}

func NewProgressReporter(total int64) *ProgressReporter {
	now := time.Now()
	return &ProgressReporter{
		totalBytes:     total,
		partsCompleted: make(map[int]bool),
		partProgress:   make(map[int]int64),
		startTime:      now,
		lastUpdateTime: now,
		speedWindow: make([]struct {
			timestamp time.Time
			bytes     int64
		}, 0, 10),
	}
}

// 添加已传输的字节数
func (pr *ProgressReporter) AddBytes(n int64) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	now := time.Now()

	// 更新字节计数
	pr.currentBytes += n

	// 1. 更新速度窗口
	pr.speedWindow = append(pr.speedWindow, struct {
		timestamp time.Time
		bytes     int64
	}{now, n})

	// 2. 清理速度窗口，只保留最近 1 秒的数据
	windowStart := 0
	for i, entry := range pr.speedWindow {
		if now.Sub(entry.timestamp) <= 1*time.Second {
			windowStart = i
			break
		}
	}
	if windowStart > 0 {
		pr.speedWindow = pr.speedWindow[windowStart:]
	}

	// 3. 计算实时速度（最近 1 秒的数据）
	var windowBytes int64
	for _, entry := range pr.speedWindow {
		windowBytes += entry.bytes
	}

	if len(pr.speedWindow) > 0 {
		windowDuration := now.Sub(pr.speedWindow[0].timestamp).Seconds()
		if windowDuration > 0 {
			// 实时速度 = 窗口内总字节数 / 窗口时间跨度
			pr.instantSpeed = float64(windowBytes) / windowDuration / 1024 / 1024 // MB/s
		}
	}

	// 4. 计算平均速度（从开始到现在）
	totalDuration := now.Sub(pr.startTime).Seconds()
	if totalDuration > 0 {
		// 平均速度 = 总传输字节数 / 总时间
		pr.avgSpeed = float64(pr.currentBytes) / totalDuration / 1024 / 1024 // MB/s
	}

	// 更新时间
	pr.lastUpdateTime = now
}

// 更新特定分片的进度
func (pr *ProgressReporter) UpdatePartProgress(partNum int, bytesRead int64, measuredSpeed ...float64) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	// 获取当前分片的已读取字节数
	currentPartBytes, exists := pr.partProgress[partNum]

	// 计算新增的字节数
	var newBytes int64
	if exists {
		newBytes = bytesRead - currentPartBytes
	} else {
		newBytes = bytesRead
	}

	// 更新分片进度
	pr.partProgress[partNum] = bytesRead

	// 只有当有新的字节被读取时才更新总进度
	if newBytes > 0 {
		now := time.Now()

		// 更新总字节数
		pr.currentBytes += newBytes

		// 1. 更新速度窗口
		pr.speedWindow = append(pr.speedWindow, struct {
			timestamp time.Time
			bytes     int64
		}{now, newBytes})

		// 2. 清理速度窗口，只保留最近 1 秒的数据
		windowStart := 0
		for i, entry := range pr.speedWindow {
			if now.Sub(entry.timestamp) <= 1*time.Second {
				windowStart = i
				break
			}
		}
		if windowStart > 0 {
			pr.speedWindow = pr.speedWindow[windowStart:]
		}

		// 3. 计算实时速度（最近 1 秒的数据）
		var windowBytes int64
		for _, entry := range pr.speedWindow {
			windowBytes += entry.bytes
		}

		if len(pr.speedWindow) > 0 {
			windowDuration := now.Sub(pr.speedWindow[0].timestamp).Seconds()
			if windowDuration > 0 {
				// 实时速度 = 窗口内总字节数 / 窗口时间跨度
				pr.instantSpeed = float64(windowBytes) / windowDuration / 1024 / 1024 // MB/s
			}
		}

		// 4. 计算平均速度（从开始到现在）
		totalDuration := now.Sub(pr.startTime).Seconds()
		if totalDuration > 0 {
			// 平均速度 = 总传输字节数 / 总时间
			pr.avgSpeed = float64(pr.currentBytes) / totalDuration / 1024 / 1024 // MB/s
		}

		// 更新时间
		pr.lastUpdateTime = now
	}
}

func (pr *ProgressReporter) MarkPartComplete(partNumber int) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.partsCompleted[partNumber] = true
}

func (pr *ProgressReporter) Progress() (int64, int64, float64, float64, float64, float64) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	elapsed := time.Since(pr.startTime).Seconds()
	overallSpeed := float64(pr.currentBytes) / elapsed / 1024 / 1024 // MB/s
	percent := float64(pr.currentBytes) / float64(pr.totalBytes) * 100
	completedParts := len(pr.partsCompleted)

	// 返回当前字节数、总字节数、百分比、平均速度、瞬时速度、完成的分片数
	return pr.currentBytes, pr.totalBytes, percent, overallSpeed, pr.avgSpeed, float64(completedParts)
}

func loadConfig(path string, profileName string) (*Config, error) {
	log.Printf("Attempting to load config file: %s", path)

	// 检查文件是否存在
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("config file does not exist: %s", path)
	}

	fileContent, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	log.Printf("Successfully read config file, size: %d bytes", len(fileContent))

	// 解析整个配置文件
	var configMap map[string]interface{}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".yaml" || ext == ".yml" {
		// 使用 YAML 解析
		err = yaml.Unmarshal(fileContent, &configMap)
		if err != nil {
			return nil, fmt.Errorf("failed to parse YAML config file: %v", err)
		}
		log.Printf("Using YAML format to parse config file")
	} else {
		return nil, fmt.Errorf("unsupported config file format: %s", ext)
	}

	// 创建配置文件结构
	configFile := &ConfigFile{
		Profiles: make(map[string]EndpointConfig),
	}

	// 提取默认配置
	if defaultVal, ok := configMap["default"]; ok {
		if defaultStr, ok := defaultVal.(string); ok {
			configFile.Default = defaultStr
		}
	}

	// 提取全局配置
	if globalVal, ok := configMap["global"]; ok {
		if globalMap, ok := globalVal.(map[string]interface{}); ok {
			// 将全局配置转换为YAML并解析
			globalYaml, err := yaml.Marshal(globalMap)
			if err == nil {
				yaml.Unmarshal(globalYaml, &configFile.Global)
			}
		}
	}

	// 提取所有配置文件
	for key, value := range configMap {
		if key != "default" && key != "global" {
			if profileMap, ok := value.(map[string]interface{}); ok {
				var profile EndpointConfig
				// 将配置转换为YAML并解析
				profileYaml, err := yaml.Marshal(profileMap)
				if err == nil {
					yaml.Unmarshal(profileYaml, &profile)
					configFile.Profiles[key] = profile
				}
			}
		}
	}

	// 如果没有指定配置文件，使用默认配置
	if profileName == "" {
		profileName = configFile.Default
	}

	// 检查指定的配置是否存在
	_, ok := configFile.Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("profile '%s' not found in config file", profileName)
	}

	// 将配置文件中的配置应用到Config对象
	finalConfig := &Config{}
	if err = applyConfigFromFile(finalConfig, configFile, profileName); err != nil {
		return nil, err
	}

	// 设置状态目录
	finalConfig.StateDir = configFile.Global.StateDir

	// 设置默认值
	if finalConfig.Region == "" {
		finalConfig.Region = "auto"
	}
	if finalConfig.PartSize <= 0 {
		finalConfig.PartSize = 32 // 默认 32MB
	}
	if finalConfig.Concurrency <= 0 {
		finalConfig.Concurrency = 4 // 默认 4 个并发
	}

	// 打印加载的配置信息用于调试
	log.Printf("Loaded profile '%s' - AccessKey: %s, Endpoint: %s",
		profileName, maskString(finalConfig.AccessKey), finalConfig.Endpoint)

	return finalConfig, nil
}

// 检查文件是否存在
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// 将配置文件中的配置应用到Config对象
func applyConfigFromFile(cfg *Config, configFile *ConfigFile, profileName string) error {
	// 如果未指定配置文件，则使用默认配置
	if profileName == "" {
		profileName = configFile.Default
	}

	// 获取指定的配置
	profileConfig, ok := configFile.Profiles[profileName]
	if !ok {
		return fmt.Errorf("profile '%s' not found in config file", profileName)
	}

	// 先应用全局配置作为默认值
	cfg.JSONOutput = configFile.Global.JSONOutput
	// 应用全局配置的通用参数（如果设置了）
	if configFile.Global.PartSize > 0 {
		cfg.PartSize = configFile.Global.PartSize
	}
	if configFile.Global.Concurrency > 0 {
		cfg.Concurrency = configFile.Global.Concurrency
	}
	if configFile.Global.Proxy != "" {
		cfg.Proxy = configFile.Global.Proxy
	}
	if configFile.Global.Region != "" {
		cfg.Region = configFile.Global.Region
	}
	if configFile.Global.Resume {
		cfg.ResumeUpload = true
	}
	if configFile.Global.AddressingStyle != "" {
		cfg.AddressingStyle = configFile.Global.AddressingStyle
	}
	if configFile.Global.Insecure {
		cfg.Insecure = true
	}

	// 然后应用特定配置文件的设置（覆盖全局配置）
	cfg.AccessKey = profileConfig.AccessKey
	cfg.SecretKey = profileConfig.SecretKey
	cfg.Endpoint = profileConfig.Endpoint
	if profileConfig.Bucket != "" {
		cfg.Bucket = profileConfig.Bucket
	}
	if profileConfig.Region != "" {
		cfg.Region = profileConfig.Region
	}
	if profileConfig.Proxy != "" {
		cfg.Proxy = profileConfig.Proxy
	}
	if profileConfig.PartSize > 0 {
		cfg.PartSize = profileConfig.PartSize
	}
	if profileConfig.Concurrency > 0 {
		cfg.Concurrency = profileConfig.Concurrency
	}
	if profileConfig.ResumeUpload {
		cfg.ResumeUpload = true
	}
	if profileConfig.AddressingStyle != "" {
		cfg.AddressingStyle = profileConfig.AddressingStyle
	}
	if profileConfig.Insecure {
		cfg.Insecure = true
	}

	return nil
}

// 隐藏敏感信息的安全函数
func maskString(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + "****" + s[len(s)-2:]
}

func createTransport(proxyURL string) (http.RoundTripper, error) {
	if proxyURL == "" {
		return http.DefaultTransport, nil
	}

	log.Printf("Attempting to set proxy: %s", proxyURL)

	// 解析代理 URL
	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("unable to parse proxy URL: %v", err)
	}

	// 处理不同类型的代理
	switch parsedURL.Scheme {
	case "http", "https":
		// HTTP/HTTPS 代理
		return &http.Transport{
			Proxy: http.ProxyURL(parsedURL),
		}, nil

	case "socks", "socks5":
		// SOCKS 代理
		dialer, err := proxy.SOCKS5("tcp", parsedURL.Host, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to create SOCKS proxy: %v", err)
		}

		return &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported proxy protocol: %s", parsedURL.Scheme)
	}
}

// 生成状态文件的路径
func getStateFilePath(cfg *Config, filePath string) string {
	// 使用文件的绝对路径和文件大小生成唯一的状态文件名
	absPath, _ := filepath.Abs(filePath)
	fileInfo, _ := os.Stat(filePath)
	fileID := fmt.Sprintf("%s_%d", absPath, fileInfo.Size())
	hash := md5.Sum([]byte(fileID))
	hashStr := hex.EncodeToString(hash[:])

	// 如果没有指定 StateDir，则使用系统临时目录
	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = os.TempDir()
	}

	return filepath.Join(stateDir, fmt.Sprintf("s3upload_%s.json", hashStr))
}

// 保存上传状态
func saveUploadState(cfg *Config, state *UploadState) error {
	if !cfg.ResumeUpload {
		return nil // 如果未启用断点续传，则不保存状态
	}

	statePath := getStateFilePath(cfg, state.FilePath)

	// 确保目录存在
	stateDir := filepath.Dir(statePath)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %v", err)
	}

	// 更新最后修改时间
	state.LastModified = time.Now()

	// 将状态序列化为 JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize upload state: %v", err)
	}

	// 写入文件
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write upload state file: %v", err)
	}

	log.Printf("Upload state saved to: %s", statePath)
	return nil
}

// 加载上传状态
func loadUploadState(cfg *Config, filePath string) (*UploadState, error) {
	if !cfg.ResumeUpload {
		return nil, nil // 如果未启用断点续传，则不加载状态
	}

	statePath := getStateFilePath(cfg, filePath)

	// 检查状态文件是否存在
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		return nil, nil // 文件不存在，返回 nil
	}

	// 读取状态文件
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read upload state file: %v", err)
	}

	// 反序列化
	var state UploadState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse upload state: %v", err)
	}

	// 检查状态是否过期（超过 7 天）
	if time.Since(state.LastModified) > 7*24*time.Hour {
		log.Printf("Upload state has expired, starting a new upload")
		os.Remove(statePath) // 删除过期的状态文件
		return nil, nil
	}

	// 检查文件是否已经被修改
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %v", err)
	}

	if fileInfo.Size() != state.FileSize {
		log.Printf("File size has changed, starting a new upload")
		os.Remove(statePath) // 删除无效的状态文件
		return nil, nil
	}

	log.Printf("Upload state loaded, continuing upload of %s, %d parts completed",
		state.Key, len(state.CompletedParts))
	return &state, nil
}

// 清除上传状态
func clearUploadState(cfg *Config, filePath string) error {
	if !cfg.ResumeUpload {
		return nil
	}

	statePath := getStateFilePath(cfg, filePath)

	// 删除状态文件
	if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete upload state file: %v", err)
	}

	log.Printf("Upload state cleared: %s", statePath)
	return nil
}

func uploadFile(client *s3.Client, cfg *Config, filePath string, key string, reporter *ProgressReporter) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// 如果没有提供key，则使用文件名
	if key == "" {
		key = filepath.Base(filePath)
	}

	// 获取文件信息
	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file info: %v", err)
	}
	fileSize := stat.Size()
	partSize := cfg.PartSize * 1024 * 1024 // 转换为字节
	numParts := (fileSize + partSize - 1) / partSize

	// 尝试加载上传状态（断点续传）
	var uploadID string
	var partETags []types.CompletedPart

	// 检查是否有之前的上传状态
	state, err := loadUploadState(cfg, filePath)
	if err != nil {
		log.Printf("Failed to load upload state: %v, starting a new upload", err)
	}

	// 如果找到有效的上传状态，使用它继续上传
	if state != nil && state.Key == key && state.PartSize == partSize {
		log.Printf("Continuing previous upload, %d/%d parts completed", len(state.CompletedParts), numParts)
		uploadID = state.UploadID
		partETags = state.CompletedParts

		// 更新进度报告器
		var completedBytes int64
		for _, part := range state.CompletedParts {
			if part.PartNumber != nil {
				partNum := *part.PartNumber
				if partNum > 0 && partNum <= int32(numParts) {
					completedSize := partSize
					if partNum == int32(numParts) {
						completedSize = fileSize - (int64(partNum)-1)*partSize
					}
					reporter.partProgress[int(partNum)] = completedSize
					completedBytes += completedSize
				}
			}
		}
		reporter.currentBytes = completedBytes
	} else {
		// 创建新的分段上传
		log.Printf("Starting new upload: %s (%d MB) divided into %d parts", key, fileSize/1e6, numParts)
		createResp, err := client.CreateMultipartUpload(context.TODO(), &s3.CreateMultipartUploadInput{
			Bucket: aws.String(cfg.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return err
		}
		uploadID = *createResp.UploadId

		// 初始化分片标签数组
		partETags = make([]types.CompletedPart, int(numParts))
	}

	// 保存初始上传状态
	initialState := &UploadState{
		FilePath:       filePath,
		Key:            key,
		UploadID:       uploadID,
		FileSize:       fileSize,
		PartSize:       partSize,
		CompletedParts: partETags,
		LastModified:   time.Now(),
	}
	if err := saveUploadState(cfg, initialState); err != nil {
		log.Printf("Failed to save upload state: %v", err)
	}

	var wg sync.WaitGroup
	mutex := &sync.Mutex{}

	// 限制并发数
	semaphore := make(chan struct{}, cfg.Concurrency)
	errChan := make(chan error, int(numParts))

	// 创建一个集合来跟踪需要上传的分片
	partsToUpload := make(map[int64]bool)
	for partNumber := int64(1); partNumber <= numParts; partNumber++ {
		// 检查这个分片是否已经完成
		alreadyCompleted := false
		if partETags != nil {
			for _, part := range partETags {
				if part.PartNumber != nil && *part.PartNumber == int32(partNumber) && part.ETag != nil {
					alreadyCompleted = true
					break
				}
			}
		}

		if !alreadyCompleted {
			partsToUpload[partNumber] = true
		}
	}

	// 如果所有分片已经上传完成，直接完成上传
	if len(partsToUpload) == 0 {
		log.Printf("All parts already uploaded, completing upload directly")
		// 完成上传
		_, err = client.CompleteMultipartUpload(context.TODO(), &s3.CompleteMultipartUploadInput{
			Bucket:          aws.String(cfg.Bucket),
			Key:             aws.String(key),
			UploadId:        aws.String(uploadID),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: partETags},
		})
		if err != nil {
			return fmt.Errorf("完成上传失败: %v", err)
		}

		// 清除上传状态
		clearUploadState(cfg, filePath)
		return nil
	}

	// 对需要上传的分片进行上传
	for partNumber := range partsToUpload {
		wg.Add(1)
		semaphore <- struct{}{} // 获取信号量

		go func(partNum int64) {
			defer wg.Done()
			defer func() { <-semaphore }() // 释放信号量

			// 最多重试 3 次
			maxRetries := 3
			var lastErr error

			for retry := 0; retry < maxRetries; retry++ {
				// 如果是重试，等待一段时间
				if retry > 0 {
					backoff := time.Duration(retry*2) * time.Second
					log.Printf("Part %d upload failed, retry %d, waiting %v...", partNum, retry, backoff)
					time.Sleep(backoff)
				}

				// 计算当前分片的起始位置和大小
				startPos := (partNum - 1) * partSize
				size := partSize
				if partNum == numParts {
					size = fileSize - startPos // 最后一个分片可能较小
				}

				// 创建一个新的读取器，从正确的位置开始读取
				partFile, err := os.Open(filePath)
				if err != nil {
					lastErr = fmt.Errorf("error opening file for part %d: %v", partNum, err)
					continue // 重试
				}
				defer partFile.Close()

				// 移动到正确的位置
				_, err = partFile.Seek(startPos, 0)
				if err != nil {
					lastErr = fmt.Errorf("error seeking to position for part %d: %v", partNum, err)
					continue // 重试
				}

				// 读取当前分片的数据
				buffer := make([]byte, size)
				n, err := io.ReadFull(partFile, buffer)
				if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
					lastErr = fmt.Errorf("error reading part %d: %v", partNum, err)
					continue // 重试
				}

				// 创建一个带进度监控的读取器
				// bytes.NewReader 返回的已经是 io.ReadSeeker
				progressReader := &ProgressReader{
					Reader:   bytes.NewReader(buffer[:n]),
					Reporter: reporter,
					PartNum:  int(partNum),
					Size:     int64(n),
				}

				// 上传分片
				resp, err := client.UploadPart(context.TODO(), &s3.UploadPartInput{
					Bucket:     aws.String(cfg.Bucket),
					Key:        aws.String(key),
					PartNumber: aws.Int32(int32(partNum)),
					UploadId:   aws.String(uploadID),
					Body:       progressReader,
				})

				if err != nil {
					lastErr = fmt.Errorf("error uploading part %d: %v", partNum, err)
					log.Printf("Part %d upload failed: %v", partNum, err)
					continue // 重试
				}

				// 标记分片完成
				reporter.MarkPartComplete(int(partNum))

				mutex.Lock()
				partETags[partNum-1] = types.CompletedPart{
					ETag:       resp.ETag,
					PartNumber: aws.Int32(int32(partNum)),
				}
				mutex.Unlock()

				// 更新上传状态文件
				state := &UploadState{
					FilePath:       filePath,
					Key:            key,
					UploadID:       uploadID,
					FileSize:       fileSize,
					PartSize:       partSize,
					CompletedParts: partETags,
					LastModified:   time.Now(),
				}
				saveUploadState(cfg, state)

				// 上传成功，跳出重试循环
				break
			}

			// 如果所有重试都失败了，将最后一个错误发送到错误通道
			if lastErr != nil {
				errChan <- lastErr
			}
		}(partNumber)
	}

	// 等待所有上传完成
	wg.Wait()
	close(errChan)

	// 检查是否有错误发生
	for err := range errChan {
		if err != nil {
			// 尝试中止上传
			_, abortErr := client.AbortMultipartUpload(context.TODO(), &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(cfg.Bucket),
				Key:      aws.String(key),
				UploadId: aws.String(uploadID),
			})
			if abortErr != nil {
				log.Printf("Failed to abort multipart upload: %v", abortErr)
			}
			return err
		}
	}

	// 完成上传
	_, err = client.CompleteMultipartUpload(context.TODO(), &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(cfg.Bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: partETags},
	})
	if err != nil {
		return fmt.Errorf("完成上传失败: %v", err)
	}

	// 清除上传状态
	clearUploadState(cfg, filePath)

	return nil
}

func monitorProgress(reporter *ProgressReporter, jsonOutput bool) {
	// 使用更短的间隔更新进度
	ticker := time.NewTicker(100 * time.Millisecond) // 提高更新频率
	defer ticker.Stop()

	// 创建一个通道用于停止监控
	done := make(chan struct{})

	// 启动一个 goroutine 来检查上传是否完成
	go func() {
		for {
			_, total, percent, _, _, _ := reporter.Progress()
			if percent >= 100.0 || total == 0 {
				close(done)
				return
			}
			time.Sleep(50 * time.Millisecond) // 提高检查频率
		}
	}()

	// 记录上次更新时间，避免更新过于频繁
	lastPrintTime := time.Now()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			// 限制控制台输出频率，避免闪烁太快
			if now.Sub(lastPrintTime) < 100*time.Millisecond && !jsonOutput {
				continue
			}

			curr, total, percent, avgSpeed, instantSpeed, completedParts := reporter.Progress()

			if jsonOutput {
				progress := struct {
					UploadedBytes        int64   `json:"uploaded_bytes"`
					TotalBytes           int64   `json:"total_bytes"`
					Percent              float64 `json:"percent"`
					AvgSpeedMBPerSec     float64 `json:"avg_speed_mbps"`
					InstantSpeedMBPerSec float64 `json:"instant_speed_mbps"`
					CompletedParts       float64 `json:"completed_parts"`
					TotalParts           int     `json:"total_parts"`
					EstimatedTimeLeft    string  `json:"estimated_time_left"`
				}{
					curr, total, percent, avgSpeed, instantSpeed, completedParts,
					int((total + 64*1024*1024 - 1) / (64 * 1024 * 1024)),
					formatTimeLeft(total-curr, avgSpeed),
				}
				jsonData, _ := json.MarshalIndent(progress, "", "  ")
				fmt.Println(string(jsonData))
			} else {
				// 计算剩余时间
				timeLeft := formatTimeLeft(total-curr, avgSpeed)

				// 使用\r返回到行首并覆盖当前行
				fmt.Printf("\rUpload progress: %.2f%% (%d/%d MB) Speed: %.2f MB/s Avg: %.2f MB/s Parts: %.0f ETA: %s",
					percent, curr/1e6, total/1e6, instantSpeed, avgSpeed, completedParts, timeLeft)
			}

			lastPrintTime = now

		case <-done:
			// 上传完成，退出监控
			return
		}
	}
}

// 格式化剩余时间
func formatTimeLeft(bytesLeft int64, speedMBps float64) string {
	if speedMBps <= 0 {
		return "--:--:--"
	}

	// 计算剩余秒数
	secondsLeft := float64(bytesLeft) / (speedMBps * 1024 * 1024)

	// 转换为时分秒格式
	hours := int(secondsLeft) / 3600
	minutes := (int(secondsLeft) % 3600) / 60
	seconds := int(secondsLeft) % 60

	if hours > 0 {
		return fmt.Sprintf("%dH%dm%ds", hours, minutes, seconds)
	} else if minutes > 0 {
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	} else {
		return fmt.Sprintf("%ds", seconds)
	}
}

// ProgressReader 是一个带进度监控的 io.ReadSeeker 实现
type ProgressReader struct {
	Reader     io.ReadSeeker // 改为 ReadSeeker 以支持 Seek 操作
	Reporter   *ProgressReporter
	PartNum    int
	Size       int64
	BytesRead  int64
	lastUpdate time.Time // 上次更新时间
	lastBytes  int64     // 上次已读字节数
}

// Read 实现 io.Reader 接口
func (pr *ProgressReader) Read(p []byte) (int, error) {
	// 记录当前时间
	now := time.Now()

	// 读取数据
	n, err := pr.Reader.Read(p)

	if n > 0 {
		// 更新已读字节数
		pr.BytesRead += int64(n)

		// 控制更新频率，避免更新过于频繁
		if pr.lastUpdate.IsZero() || now.Sub(pr.lastUpdate) >= 50*time.Millisecond {
			// 计算增量和速度
			bytesIncrement := pr.BytesRead - pr.lastBytes
			timeElapsed := now.Sub(pr.lastUpdate).Seconds()

			// 更新进度
			if !pr.lastUpdate.IsZero() && timeElapsed > 0 {
				// 计算实际网络传输速度
				speed := float64(bytesIncrement) / timeElapsed / 1024 / 1024 // MB/s
				pr.Reporter.UpdatePartProgress(pr.PartNum, pr.BytesRead, speed)
			} else {
				// 首次读取，不计算速度
				pr.Reporter.UpdatePartProgress(pr.PartNum, pr.BytesRead, 0)
			}

			// 更新上次读取时间和字节数
			pr.lastUpdate = now
			pr.lastBytes = pr.BytesRead
		}
	}

	return n, err
}

// Seek 实现 io.Seeker 接口
func (pr *ProgressReader) Seek(offset int64, whence int) (int64, error) {
	// 重置已读字节数，因为可能会重新定位
	if whence == io.SeekStart && offset == 0 {
		pr.BytesRead = 0
	}
	return pr.Reader.Seek(offset, whence)
}

// parseRemotePath parses remote path with flexible format
// Supports:
// 1. profile:bucket:path - format with profile and bucket
// 2. /path or path - pure path format (profile and bucket from config/flags)
func parseRemotePath(remotePath string) (profile, bucket, key string, err error) {
	// 检查是否是纯路径格式 (不包含冒号)
	if !strings.Contains(remotePath, ":") {
		// 纯路径格式，profile和bucket将从配置或命令行参数获取
		return "", "", strings.TrimPrefix(remotePath, "/"), nil
	}

	// 检查是否是 profile:bucket:path 格式
	parts := strings.SplitN(remotePath, ":", 3)
	if len(parts) == 3 {
		// profile:bucket:path 格式
		profile = parts[0]
		bucket = parts[1]
		// 去除前导斜杠
		key = strings.TrimPrefix(parts[2], "/")
		return profile, bucket, key, nil
	}

	// 不支持其他格式
	return "", "", "", fmt.Errorf("invalid remote path format, should be 'profile:bucket:path' or just 'path'")
}

// getDefaultConfigPath returns the path to the default config file
func getDefaultConfigPath() string {
	// 首先尝试获取可执行文件所在目录的config.yaml
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		configPath := filepath.Join(exeDir, "config.yaml")
		if _, err := os.Stat(configPath); err == nil {
			return configPath
		}
	}

	// 然后尝试用户主目录
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".s3uploader", "config.yaml")
}

// 自定义日志输出函数
var isVerbose bool
var isQuiet bool

// verboseLog 只在verbose模式下输出日志
func verboseLog(format string, v ...interface{}) {
	if isVerbose && !isQuiet {
		log.Printf(format, v...)
	}
}

// infoLog 在非安静模式下输出日志
func infoLog(format string, v ...interface{}) {
	if !isQuiet {
		log.Printf(format, v...)
	}
}

// errorLog 始终输出错误日志
func errorLog(format string, v ...interface{}) {
	log.Printf(format, v...)
}

func main() {
	// Define flags
	configFile := flag.String("config", "", "Config file path (YAML)")
	profile := flag.String("profile", "", "Profile name in config file")
	jsonOutput := flag.Bool("json", false, "Output progress in JSON format")
	partSize := flag.Int64("part-size", 0, "Part size in MB")
	concurrency := flag.Int("concurrency", 0, "Upload concurrency")
	accessKey := flag.String("access-key", "", "Access key")
	secretKey := flag.String("secret-key", "", "Secret key")
	endpoint := flag.String("endpoint", "", "Endpoint URL")
	bucket := flag.String("bucket", "", "Bucket name")
	region := flag.String("region", "auto", "Region (default: auto)")
	proxy := flag.String("proxy", "", "Proxy server URL (optional, like: socks://127.0.0.1:7890 or http://127.0.0.1:1080)")
	resume := flag.Bool("resume", false, "Enable resume upload")
	quiet := flag.Bool("quiet", false, "Suppress non-essential output")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	pathStyle := flag.Bool("path-style", true, "Use path-style addressing (default: true)")
	virtualStyle := flag.Bool("virtual-style", false, "Use virtual-hosted-style addressing")
	insecure := flag.Bool("insecure", false, "Skip TLS verification")

	// Parse flags
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <destination> <local/file/path>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Destination formats:\n")
		fmt.Fprintf(os.Stderr, "  profile:bucket:path - Format with profile and bucket\n")
		fmt.Fprintf(os.Stderr, "  /path or path       - Pure path (profile from --profile or default, bucket from --bucket or config)\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	// 检查命令行参数
	args := flag.Args()
	if len(args) != 2 {
		flag.Usage()
		log.Fatal("Please provide destination and source file paths")
	}

	remotePath := args[0]
	filePath := args[1]

	// 解析远程路径
	pathProfile, bucketName, objectKey, err := parseRemotePath(remotePath)
	if err != nil {
		errorLog("Failed to parse remote path: %v", err)
		os.Exit(1)
	}

	// 如果命令行指定了profile，使用命令行的profile
	if *profile != "" {
		pathProfile = *profile
	}

	// 创建远程对象键
	remoteKey := filepath.Base(filePath)
	if objectKey != "" {
		// 如果远程路径中指定了路径，则使用该路径作为前缀
		// 确保路径结尾有斜杠
		if !strings.HasSuffix(objectKey, "/") {
			objectKey = objectKey + "/"
		}
		remoteKey = objectKey + filepath.Base(filePath)
		verboseLog("Using object key with prefix: %s", remoteKey)
	}

	// 初始化配置
	var cfg *Config

	// 加载配置文件
	if *configFile != "" {
		// 使用指定的配置文件
		absPath, err := filepath.Abs(*configFile)
		if err != nil {
			errorLog("Unable to get absolute path for config file: %v", err)
			os.Exit(1)
		}
		verboseLog("Attempting to load from config file: %s", absPath)

		cfg, err = loadConfig(absPath, pathProfile)
		if err != nil {
			errorLog("Failed to load config file: %v", err)
			os.Exit(1)
		}
		verboseLog("Successfully loaded configuration from config file")
		infoLog("Loaded configuration from: %s", absPath)
	} else {
		// 尝试加载默认配置文件
		defaultConfigPath := getDefaultConfigPath()
		if defaultConfigPath != "" && fileExists(defaultConfigPath) {
			verboseLog("Attempting to load from default config file: %s", defaultConfigPath)
			cfg, err = loadConfig(defaultConfigPath, pathProfile)
			if err != nil {
				verboseLog("Warning: Failed to load default config file: %v", err)
				// 创建一个新的配置
				cfg = &Config{}
			} else {
				verboseLog("Successfully loaded configuration from default config file")
				infoLog("Loaded configuration from: %s", defaultConfigPath)
			}
		} else {
			// 否则创建一个新的配置
			cfg = &Config{}
			verboseLog("No config file specified, using command line parameters and default values")
		}
	}

	// 设置默认值
	if cfg.PartSize <= 0 {
		cfg.PartSize = 32 // 默认 32MB
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4 // 默认 4 个并发
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}

	// 命令行参数覆盖配置文件
	if *accessKey != "" {
		cfg.AccessKey = *accessKey
	}
	if *secretKey != "" {
		cfg.SecretKey = *secretKey
	}
	if *endpoint != "" {
		cfg.Endpoint = *endpoint
	}
	if *bucket != "" {
		// 命令行参数优先
		cfg.Bucket = *bucket
		verboseLog("Using bucket from command line: %s", *bucket)
	} else if bucketName != "" {
		// 然后是远程路径中的bucket
		cfg.Bucket = bucketName
		verboseLog("Using bucket from remote path: %s", bucketName)
	} else if cfg.Bucket == "" {
		// 如果都没有指定，则报错
		errorLog("Bucket name must be provided either in config, command line, or in remote path")
		os.Exit(1)
	}

	// 显示存储桶信息
	infoLog("Using bucket: %s", cfg.Bucket)
	if *region != "auto" {
		cfg.Region = *region
	}
	if *proxy != "" {
		cfg.Proxy = *proxy
	}
	if *partSize > 0 {
		cfg.PartSize = *partSize
	}
	if *concurrency > 0 {
		cfg.Concurrency = *concurrency
	}
	if *resume {
		cfg.ResumeUpload = true
	}
	if *jsonOutput {
		cfg.JSONOutput = true
	}

	// 验证必要的配置
	if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Endpoint == "" {
		errorLog("Missing required configuration parameters: access-key, secret-key, endpoint must be provided")
		os.Exit(1)
	}

	if cfg.Bucket == "" {
		errorLog("Bucket name must be provided either in config or in remote path")
		os.Exit(1)
	}

	// Setup Proxy
	transport, err := createTransport(cfg.Proxy)
	if err != nil {
		errorLog("Failed to setup proxy: %v", err)
		os.Exit(1)
	}

	// 显示代理信息
	if cfg.Proxy != "" {
		infoLog("Using proxy: %s", cfg.Proxy)
	}

	// 创建自定义配置
	infoLog("Using endpoint: %s", cfg.Endpoint)

	// 设置日志模式
	isQuiet = *quiet
	isVerbose = *verbose

	// 应用quiet和verbose标志
	if isQuiet {
		// 在安静模式下，只在错误时输出日志
		log.SetOutput(io.Discard) // 禁止非必要输出
	} else if isVerbose {
		// 在verbose模式下，输出完整日志
		infoLog("Verbose logging enabled")
	}

	// 创建基本的 AWS 配置
	awsCfg := aws.Config{
		Region: cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKey,
			cfg.SecretKey,
			"",
		),
		EndpointResolverWithOptions: aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:           cfg.Endpoint,
					SigningRegion: cfg.Region,
					Source:        aws.EndpointSourceCustom,
				}, nil
			},
		),
	}

	// 如果配置了代理或需要跳过TLS验证，自定义HTTP客户端
	if transport != nil || cfg.Insecure || *insecure {
		// 创建基本的Transport
		var customTransport http.RoundTripper
		if transport != nil {
			customTransport = transport
			verboseLog("Using proxy configuration: %s", cfg.Proxy)
		} else {
			customTransport = http.DefaultTransport.(*http.Transport).Clone()
		}

		// 如果需要跳过TLS验证
		if cfg.Insecure || *insecure {
			if httpTransport, ok := customTransport.(*http.Transport); ok {
				if httpTransport.TLSClientConfig == nil {
					httpTransport.TLSClientConfig = &tls.Config{}
				}
				httpTransport.TLSClientConfig.InsecureSkipVerify = true
				verboseLog("TLS verification disabled")
			}
		}

		// 设置客户端
		awsCfg.HTTPClient = &http.Client{
			Transport: customTransport,
		}
	}

	// 创建 S3 客户端
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		// 设置寻址方式
		if cfg.AddressingStyle == PathStyle || *pathStyle {
			o.UsePathStyle = true
			verboseLog("Using path-style addressing")
		} else if cfg.AddressingStyle == VirtualHostedStyle || *virtualStyle {
			o.UsePathStyle = false
			verboseLog("Using virtual-hosted-style addressing")
		}
	})

	// 获取文件大小
	fileSize := getFileSize(filePath)

	// Start Progress Monitor
	reporter := NewProgressReporter(fileSize)

	// 显示分片信息
	parts := calculatePartCount(fileSize, cfg.PartSize*1024*1024)
	verboseLog("Starting upload: %s (%s) divided into %d parts", remoteKey, formatBytes(fileSize), parts)

	// 启动进度监控
	go monitorProgress(reporter, cfg.JSONOutput)

	// 记录开始时间
	startTime := time.Now()

	// Upload File
	err = uploadFile(client, cfg, filePath, remoteKey, reporter)
	if err != nil {
		errorLog("Upload failed: %v", err)
		os.Exit(1)
	}

	// 计算总耗时和平均速度
	elapsedTime := time.Since(startTime)
	avgSpeed := float64(fileSize) / elapsedTime.Seconds() / 1024 / 1024

	// 显示成功信息
	fmt.Printf("\nUpload completed successfully. Total: %s, Time: %s, Avg Speed: %.2f MB/s\n",
		formatBytes(fileSize),
		formatDuration(elapsedTime),
		avgSpeed)
}

// 计算分片数量
func calculatePartCount(fileSize int64, partSize int64) int {
	if partSize <= 0 {
		partSize = 32 * 1024 * 1024 // 默认 32MB
	}
	parts := int(fileSize / partSize)
	if fileSize%partSize > 0 {
		parts++
	}
	return parts
}

// 格式化文件大小
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// 格式化持续时间
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	} else if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func getFileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
