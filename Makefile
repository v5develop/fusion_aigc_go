# 默认入口（对齐 unionec_user_api 的 Makefile / Makefile_l / Makefile_x 三文件设计）：
#   本地开发 / 代码生成 / 单镜像 / 部署包  → 本文件（转发到 Makefile_l）
#   每服务独立镜像 + 私有仓库发布链路        → make -f Makefile_x <target>
#
# 直接用 `make build` / `make run-all` / `make image ...` 等，等价于 make -f Makefile_l。
# 同名 target 靠 -f 区分（对齐 unionec_user_api）：
#   make media / make ops / make svc SERVICE=... BIN=...  → 本文件 → Dockerfile.media/.ops/.svc（本机原生）
#   make -f Makefile_x media / ops / ...                  → 跨编译 linux/amd64（_Dockerfile.*）
# 其余发布链路 target（push-all / deploy / ffmpeg-base 等）在 Makefile_x，需显式 -f。
include Makefile_l
