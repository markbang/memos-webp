# Fork: 上传即压缩 WebP

基于上游 `usememos/memos` **v0.31.0** 的个人分支，唯一目标：**图片在进入存储（本地 / S3 / R2）之前被压缩**，解决手机原图（4032px+、5~10MB）导致加载慢的问题。

实测：9.7MB / 3072×4096 手机原图 → **100KB / 1440×1920 WebP**（约 97× 缩小），耗时 ~0.4s。

## 改了什么

| 文件 | 说明 |
| --- | --- |
| `server/api/v1/attachment_image_compress.go` | **新增**。核心压缩函数：`imaging` 解码（自动 EXIF 翻转）→ 长边超限则 Lanczos 缩小 → `webp-go-pure` 无损度有损编码 |
| `server/api/v1/attachment_service.go` | `processAndSaveAttachment` 上传链路（一次性 + 分片上传共用）：EXIF 剥离块前面插入「压缩优先、剥离回退」流水线，并同步修正客户端上报的图片尺寸 |
| `go.mod` / `go.sum` | 新增纯 Go 依赖 `github.com/SeriousBug/webp-go-pure v1.2.0`（无 cgo） |
| `web/src/components/MemoEditor/**`（types/attachment.ts / hooks/useFileUpload.ts / services/uploadService.ts / index.tsx） | 选图时立即把文件字节快照进内存（≤64MB），分片上传从快照切片，修复批量发图偶发 `NotReadableError`（浏览器回收句柄导致） |
| `internal/motionphoto/transcode.go` | **新增**。Motion Photo 内嵌片段转码为 H.264/MP4：Android 原片多为 HEVC，Chrome/Firefox 无法解码，LIVE 预览会「有徽标但播不动」 |
| `server/fileserver/fileserver.go` | `?motion=true` 提取后先转码再缓存（缓存名升级为 `<uid>.v2.mp4`）；ffmpeg 缺失或转码失败时回退原片段 |
| `web/src/utils/attachment.ts` | Motion 片段 URL 加 `v=2`，避开 CDN/浏览器里旧的 HEVC 缓存 |
| `server/rss/rss.go` | **新增（恢复）**。上游 v0.31 删掉了 RSS（#6243），这里把 `/explore/rss.xml`、`/u/:username/rss.xml` 恢复回来，保留 ETag/Last-Modified 与服务端缓存 |
| `markdown/**` | 恢复 `RenderHTML` 以及 tag / mention / email 的 HTML 渲染器：上游为了让 RSS 消失，把这些一并删了 |
| `server/server.go`、`server/frontend/frontend.go` | 注册 RSS 路由，并让 SPA fallback 放行 `*/rss.xml` |

## 行为规则

- 处理类型：`image/jpeg|jpg|png|webp`。
- 有损 WebP，quality 85、effort 4；长边上限 1920。
- 结果一定更小才采纳（除非做了降采样——降尺寸本身就是目的）。
- **EXIF（GPS/设备信息）随重编码天然剥离**，自动旋转方向已烘焙进像素。
- 跳过：Android Motion Photo 容器（内含视频，转码会毁掉动图/视频）、GIF/动图、`heic/heif/tiff`（走原有 EXIF 剥离回退）。
- 任何处理失败都**回退上传原图**，绝不导致上传失败。
- 处理槽信号量（并发 2）复用上游已有设施，无新增风险面。
- Motion Photo（Live）播放：内嵌视频统一转 H.264（`-preset veryfast -crf 23`，去掉音轨与元数据），结果缓存在 `.motion_cache/<uid>.v2.mp4`，只转一次。
- 转码依赖系统 `ffmpeg`（镜像已内置）。没有 ffmpeg 时照旧返回原始片段，不会让请求失败。
- RSS 只在实例为 public 访问模式时可用；private 模式返回 404（与上游 v0.30 行为一致）。

## 配置（环境变量，可省）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `MEMOS_IMAGE_MAX_DIMENSION` | `1920` | 长边像素上限 |
| `MEMOS_IMAGE_WEBP_QUALITY` | `85` | 有损质量 1..100 |

## 构建

与上游完全一致（`CGO_ENABLED=0`，双架构可用）：

```bash
# 先构建前端（上游 Dockerfile 依赖 web/dist）
pnpm install && pnpm build
# 或直接按上游 scripts/Dockerfile 构建镜像
docker build -f scripts/Dockerfile -t memos-webp .
```

## 发布镜像（ghcr.io/markbang/memos-webp）

GitHub Actions（`.github/workflows/build-publish-webp.yml`）已配好：

- **手动触发**：仓库 Actions 页 → *Build and Publish WebP Image* → Run workflow（tag 默认 `0.31.1-webp`）；
- **自动触发**：推送 `v*` tag。

流程 = pnpm 构建前端 → buildx 双架构（linux/amd64 + linux/arm64）→ 直接推 ghcr 并打 manifest。

本地手动发布（等价）：

```bash
cd web && pnpm install && pnpm release && cd ..
docker build -f scripts/Dockerfile -t ghcr.io/markbang/memos-webp:0.31.1-webp .
docker push ghcr.io/markbang/memos-webp:0.31.1-webp
```

## 存量迁移工具 `tools/r2-webp-migrate`

只对新上传生效；历史上 R2 里已有的原图用本工具批量处理。规则与上传链路一致，**原地覆盖同一 key**（预签名 URL、数据库记录全部不用动）。Motion 容器/动图/无法解码的对象自动跳过。

```bash
go build -o r2-webp-migrate ./tools/r2-webp-migrate

# 先 dry-run 看清单
./r2-webp-migrate \
  --endpoint https://<account>.r2.cloudflarestorage.com \
  --bucket <bucket> \
  --prefix <key前缀> \
  --access-key <AK> --secret-key <SK> \
  --dry-run --limit 20

# 正式执行
./r2-webp-migrate \
  --endpoint https://<account>.r2.cloudflarestorage.com \
  --bucket <bucket> --prefix <key前缀> \
  --access-key <AK> --secret-key <SK>
```

凭据也可用 `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`。其他参数：`--max-dim`、`--quality`、`--effort`、`--concurrency`、`--limit`。

## 已知取舍

- Motion Photo 原图（如 `MVIMG_*.jpg`）无法压缩（含内嵌视频）。若你并不需要动图播放能力，可以后续单独跑"静态化"处理。
- WebP 重编码会丢掉 ICC/HDR 色彩信息（含 macOS 截图的 Display P3、HDR gain map）。上游会为这类图在缩略图链路保留原图，本分支优先带宽，统一压缩。
- Motion 片段首次请求需要等一次转码（~2-3s），之后走缓存。
- Apple Live Photo 的 MOV 也常为 HEVC；当前只在 Android 容器路径转码，Apple 成对上传需客户端提供可播放的 H.264 片段。
- 上游升级时：`git fetch upstream && git rebase upstream/v0.31.x`，冲突集中在压缩与 motion 转码相关文件。
