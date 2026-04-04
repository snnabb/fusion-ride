# ⚡ FusionRide 

FusionRide 是一个专为 Emby 打造的高性能、单二进制、无外部依赖的「多节点聚合代理系统」。能够将多个不同来源的 Emby 资源聚合成一个，提供原生的无缝播放体验。

![Dashboard Preview](https://github.com/user-attachments/assets/1ed705bb-ab8e-4a6c-9dd6-a05ff9ffdf93) *效果图：Apple TV 深色美学风格的现代管理面板*

## 核心特性 (Features)

*   📦 **单体终极架构**：Golang 编写，自带 Pure Go SQLite（无 CGO），前端面板完全打包为单二进制文件。部署只需要一个文件。
*   🚀 **AI 级聚合引擎**：全并发健康探测与跨服务器同源资源自动去重合并，内置**多维度码率最高优先智能调度算法**。
*   🎭 **设备终极伪装 (UA Spoofing)**：内置 `infuse`, `passthrough` (五层真实凭证降级透传), `custom` 模式，保护原站点账号不被阻断封杀。
*   📡 **无缝双轨反代**：支持流代理（Proxy - 保护服务器来源且可计量流量）和真・重定向（Redirect - 发送 302 给客户端直接以最高速读取），平滑切换。
*   💻 **沉浸式控制面板**：极客深空主题的现代 SPA，集成服务器级大盘看板，SSE 毫秒级服务器状态推送与微秒级网络流量图动态更新。

## 快速部署 (Deploy)

推荐使用 Docker Compose，最快 10 秒钟即可完成上线：

```yaml
services:
  fusionride:
    image: ghcr.io/snnabb/fusionride:latest
    # 或者直接构建: build: https://github.com/snnabb/fusion-ride.git#main
    container_name: fusionride
    restart: unless-stopped
    ports:
      - "8096:8096"
    volumes:
      - ./data:/app/data
      - ./config:/app/config
    environment:
      - TZ=Asia/Shanghai
```

## 默认运行与连接使用
1. 服务启动后访问管理面板设置初始密码：
   `http://你的IP:8096/admin/`
2. 在管理面板内添加你的任意多个上游 Emby 节点配置信息。
3. 打开任意客户端（Infuse, Emby 等）添加服务器：
   地址即为 `http://你的IP:8096` ，并且可以直接使用你上游 Emby 服务器原生的账号密码进行登录体验！

## Build (手动构建)

```bash
git clone https://github.com/snnabb/fusion-ride.git
cd fusion-ride
go build -ldflags="-s -w" -o fusionride ./cmd/fusionride
./fusionride
```

## 许可证 (License)

MIT License.
