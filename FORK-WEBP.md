# Fork: 上传即压缩 WebP

基于上游 `usememos/memos` **v0.30.0** 的个人分支，唯一目标：**图片在进入存储（本地 / S3 / R2）之前被压缩**，解决手机原图（4032px+、5~10MB）导致加载慢的问题。

实测：9.7MB / 3072×4096 手机原图 → **100KB / 1440×1920 WebP**（约 97× 缩小），耗时 ~0.4s。

## 改了什么

| 文件 | 说明 |
| --- | --- |
| `server/router/api/v1/attachment_image_compress.go` | **新增**。核心压缩函数：`imaging` 解码（自动 EXIF 翻转）→ 长边超限则 Lanczos 缩小 → `webp-go-pure` 无损度有损编码 |
| `server/router/api/v1/attachment_service.go` | `CreateAttachment` 上传链路：EXIF 剥离块替换为「压缩优先、剥离回退」流水线 |
| `go.mod` / `go.sum` | 新增纯 Go 依赖 `github.com/SeriousBug/webp-go-pure v1.2.0`（无 cgo） |
| `web/src/components/MemoEditor/**`（attachment.ts / EditorContent.tsx / useFileUpload.ts / uploadService.ts） | 选图时立即把文件字节快照进内存，保存时不再回读 File 句柄，修复批量发图偶发 `NotReadableError`（浏览器回收句柄导致） |

## 行为规则

- 处理类型：`image/jpeg|jpg|png|webp`。
- 有损 WebP，quality 85、effort 4；长边上限 1920。
- 结果一定更小才采纳（除非做了降采样——降尺寸本身就是目的）。
- **EXIF（GPS/设备信息）随重编码天然剥离**，自动旋转方向已烘焙进像素。
- 跳过：Android Motion Photo 容器（内含视频，转码会毁掉动图/视频）、GIF/动图、`heic/heif/tiff`（走原有 EXIF 剥离回退）。
- 任何处理失败都**回退上传原图**，绝不导致上传失败。
- 处理槽信号量（并发 2）复用上游已有设施，无新增风险面。

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

- **手动触发**：仓库 Actions 页 → *Build and Publish WebP Image* → Run workflow（tag 默认 `0.30.0-webp`）；
- **自动触发**：推送 `v*` tag。

流程 = pnpm 构建前端 → buildx 双架构（linux/amd64 + linux/arm64）→ 直接推 ghcr 并打 manifest。

本地手动发布（等价）：

```bash
cd web && pnpm install && pnpm release && cd ..
docker build -f scripts/Dockerfile -t ghcr.io/markbang/memos-webp:0.30.0-webp .
docker push ghcr.io/markbang/memos-webp:0.30.0-webp
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
- 上游升级时：`git fetch upstream && git rebase upstream/v0.30.x`，冲突只会落在这两个文件上。
