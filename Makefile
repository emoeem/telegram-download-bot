.PHONY: build
build:
	goreleaser build --rm-dist --single-target --snapshot
	@echo "go to '.tdl/dist' directory to see the package"

# archive/web 的 Tailwind 产物。生成物 archive/web/static/app.css 提交进仓库，
# 所以 go build / docker build 不需要 Node；只有改样式时才要跑这个目标。
# 改了 index.html / login.html 的 class 也必须重跑，否则 utility 不会被生成。
.PHONY: web
web:
	@test -d webui/node_modules || (echo "缺少 webui/node_modules，先执行: npm --prefix webui install" && exit 1)
	npm --prefix webui run build
	@echo "→ archive/web/static/app.css"

.PHONY: web-check
web-check: web
	@git diff --exit-code -- archive/web/static/app.css || \
		(echo "app.css 与源不一致：请提交重新生成的产物" && exit 1)

.PHONY: gui
gui:
	go run . gui

.PHONY: packaging
packaging:
	goreleaser release --skip-publish --auto-snapshot --rm-dist
	@echo "go to '.tdl/dist' directory to see the packages"

# ---------------------------------------------------------------- licensing
#
# 发行版必须注入校验材料，否则构建出来的软件没有授权门（开发时正是想要这个）。
# 用法：
#   make licensed TDL_LICENSE_MASTER_FILE=~/.tdl-issuer.txt
#
# 注入的是主密钥派生出的校验串与公钥，主密钥本身不会被编译进去。
LICENSE_MASTER ?= $(TDL_LICENSE_MASTER_FILE)

# 在线授权服务地址：注入后客户不需要手填服务器。
# 留空则只做离线激活（客户可用 --license-server 或激活页自行填写）。
LICENSE_SERVER ?= https://lic.emoe.eu.cc

.PHONY: license-verifier
license-verifier:
	@test -n "$(LICENSE_MASTER)" || \
		(echo "缺少 TDL_LICENSE_MASTER_FILE（先用 ./tdl license keygen --out ~/.tdl-issuer.txt）" && exit 1)
	@go run . license verifier --key-file "$(LICENSE_MASTER)"

.PHONY: licensed
licensed:
	@test -n "$(LICENSE_MASTER)" || \
		(echo "缺少 TDL_LICENSE_MASTER_FILE：发行构建必须有授权校验" && exit 1)
	$(eval LICENSE_LDFLAGS := $(shell go run . license verifier --key-file "$(LICENSE_MASTER)" | tr '\n' ' '))
	go build -ldflags "-s -w $(LICENSE_LDFLAGS) $(if $(LICENSE_SERVER),-X github.com/iyear/tdl/archive.licenceServerDefault=$(LICENSE_SERVER))" -o tdl .
	@echo "→ tdl（已注入授权校验）"

# Docker 镜像同样必须注入，否则容器里的 archive serve 没有授权门。
# 用法：make licensed-docker TDL_LICENSE_MASTER_FILE=~/.tdl-issuer.txt
.PHONY: licensed-docker
licensed-docker:
	@test -n "$(LICENSE_MASTER)" || \
		(echo "缺少 TDL_LICENSE_MASTER_FILE：发行镜像必须有授权校验" && exit 1)
	$(eval LICENSE_HMAC := $(shell go run . license verifier --key-file "$(LICENSE_MASTER)" | sed -n 's/.*hmacKeyB64=//p'))
	$(eval LICENSE_PUB := $(shell go run . license verifier --key-file "$(LICENSE_MASTER)" | sed -n 's/.*pubKeyB64=//p'))
	docker build \
		--build-arg LICENSE_HMAC=$(LICENSE_HMAC) \
		--build-arg LICENSE_PUB=$(LICENSE_PUB) \
		--build-arg LICENSE_SERVER=$(LICENSE_SERVER) \
		-t tdl-archive:licensed .
	@echo "→ 镜像 tdl-archive:licensed（已注入授权校验）"

.PHONY: licensed-aar
licensed-aar:
	@test -n "$(LICENSE_MASTER)" || \
		(echo "缺少 TDL_LICENSE_MASTER_FILE：发行构建必须有授权校验" && exit 1)
	TDL_LICENSE_MASTER_FILE="$(LICENSE_MASTER)" TDL_LICENSE_SERVER="$(LICENSE_SERVER)" bash android/build-aar.sh
