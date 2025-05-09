# S3 Uploader

一个功能强大的命令行工具，用于上传文件到兼容S3协议的存储服务，如AWS S3、Cloudflare R2、MinIO等。
注: 本项目代码大部分由 AI 实现

## 功能特点

- **代理支持**：支持HTTP和SOCKS5代理，方便在各种网络环境中使用
- **多线程上传**：支持并发分片上传，显著提高上传速度
- **多端点支持**：可以在配置文件中定义多个存储服务配置
- **断点续传**：支持中断后继续上传，避免重新上传大文件
- **灵活的路径处理**：支持多种路径格式，方便组织存储结构
- **多种寻址方式**：支持路径风格(Path-style)和虚拟主机风格(Virtual-hosted-style)寻址
- **TLS验证选项**：可选择跳过TLS验证，方便使用自签名证书的服务
- **详细的进度显示**：实时显示上传进度、速度和预计完成时间

## 安装

### 预编译二进制文件

从[发布页面](https://github.com/huanfeng/s3uploader/releases)下载适合您系统的预编译二进制文件。

### 从源代码构建

确保您已安装Go 1.16或更高版本，然后运行：

```bash
git clone https://github.com/huanfeng/s3uploader.git
cd s3uploader
go build -o s3uploader
```

## 配置

S3 Uploader使用YAML格式的配置文件。默认情况下，程序会尝试加载以下位置的配置文件：

1. 可执行文件同目录下的`config.yaml`
2. 用户主目录下的`.s3uploader/config.yaml`

您也可以使用`--config`参数指定配置文件路径。

### 配置文件示例

```yaml
# 默认使用的配置
default: p1

# 第一个配置
p1:
  access_key: your_access_key
  secret_key: your_secret_key
  endpoint: https://your-endpoint.com
  bucket: bucket1
  region: auto
  part_size: 64
  concurrency: 8
  resume: false
  proxy: "socks://127.0.0.1:7890"
  addressing_style: path
  insecure: false

# 另一个配置
p2:
  access_key: another_access_key
  secret_key: another_secret_key
  endpoint: https://another-endpoint.com
  bucket: bucket2
  region: us-east-1
  part_size: 32
  concurrency: 4
  resume: true
  addressing_style: virtual
  insecure: true

# 全局配置（适用于所有配置）
global:
  json_output: false
  addressing_style: path
  insecure: false
```

### 配置选项

| 选项 | 描述 | 默认值 |
|------|------|--------|
| `access_key` | 访问密钥 | - |
| `secret_key` | 秘密密钥 | - |
| `endpoint` | 端点URL | - |
| `bucket` | 存储桶名称 | - |
| `region` | 区域名称 | auto |
| `part_size` | 分片大小(MB) | 32 |
| `concurrency` | 并发上传的分片数 | 4 |
| `resume` | 是否启用断点续传 | false |
| `proxy` | 代理服务器URL | - |
| `addressing_style` | 寻址方式(path或virtual) | path |
| `insecure` | 是否跳过TLS验证 | false |

## 使用方法

基本用法：

```bash
s3uploader [选项] <目标> <本地文件路径>
```

目标格式：
- `profile:bucket:path` - 指定配置、存储桶和路径
- `path` - 纯路径（从配置文件或命令行参数获取配置和存储桶）

### 示例

1. 使用配置文件中的默认配置上传文件：
   ```bash
   s3uploader p1:mybucket:docs/hello.txt local/file.txt
   ```

2. 指定使用不同的配置：
   ```bash
   s3uploader --profile p2 p1:mybucket:docs/hello.txt local/file.txt
   ```

3. 使用纯路径格式：
   ```bash
   s3uploader --profile p1 --bucket mybucket docs/hello.txt local/file.txt
   ```

4. 覆盖配置文件中的设置：
   ```bash
   s3uploader --endpoint https://custom-endpoint.com --access-key KEY --secret-key SECRET p1:bucket:path/file.txt local/file.txt
   ```

5. 使用虚拟主机风格寻址：
   ```bash
   s3uploader --virtual-style p1:bucket:path/file.txt local/file.txt
   ```

6. 跳过TLS验证：
   ```bash
   s3uploader --insecure p1:bucket:path/file.txt local/file.txt
   ```

### 命令行选项

```
选项:
  --access-key string     访问密钥
  --bucket string         存储桶名称
  --concurrency int       上传并发数
  --config string         配置文件路径(YAML)
  --endpoint string       端点URL
  --insecure              跳过TLS验证
  --json                  以JSON格式输出进度
  --part-size int         分片大小(MB)
  --path-style            使用路径风格寻址(默认: true)
  --profile string        配置文件中的配置名称
  --proxy string          代理服务器URL(可选，如: socks://127.0.0.1:7890 或 http://127.0.0.1:1080)
  --quiet                 抑制非必要输出
  --region string         区域(默认: auto)
  --resume                启用断点续传
  --secret-key string     秘密密钥
  --verbose               启用详细日志
  --virtual-style         使用虚拟主机风格寻址
```

## 许可证

本项目采用MIT许可证 - 详见[LICENSE](LICENSE)文件。

## 贡献

欢迎贡献！请随时提交问题或拉取请求。

1. Fork本仓库
2. 创建您的功能分支 (`git checkout -b feature/amazing-feature`)
3. 提交您的更改 (`git commit -m 'feat: Add some amazing feature'`)
4. 推送到分支 (`git push origin feature/amazing-feature`)
5. 打开一个Pull Request
