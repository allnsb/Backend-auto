# 部署配置

本目录用于新路线的本地开发与部署配置。两套 OpenIM 替代候选已完成隔离本地功能试验，入口与复现命令见 [im-trials](im-trials/README.md)。BBS-GO 业务接入尚未完成。

配置使用独立的项目名、网络、容器及数据目录，避免与相邻 NodeBB 项目混用。公网 API 和资源访问使用 HTTPS，WebSocket 使用 WSS；当前试验仅绑定本地 loopback，不能直接作为公网部署配置。

