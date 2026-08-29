SHELL := /bin/sh

DARWIN_ARCH ?= arm64
TOOL := bin/darwin-oci
MACKER := bin/macker
ROOTFS := example/rootfs
IMAGE := example/image
PULL_LAYOUT := $(IMAGE).pulling
NGINX_ROOTFS := example/nginx-rootfs
NGINX_IMAGE := example/nginx-image
NGINX_PULL_LAYOUT := $(NGINX_IMAGE).pulling
NGINX_CONFIG := example/nginx/nginx.conf
REGISTRY_IMAGE ?=
SKOPEO ?= skopeo

.PHONY: all build macker hello image nginx-rootfs nginx nginx-run nginx-push nginx-pull inspect unpack run push pull test clean

all: image

build:
	mkdir -p bin
	go build -o $(TOOL) ./cmd/darwin-oci

macker: build
	go build -o $(MACKER) ./cmd/macker

hello:
	rm -rf $(ROOTFS)
	mkdir -p $(ROOTFS)/app
	GOOS=darwin GOARCH=$(DARWIN_ARCH) CGO_ENABLED=0 go build -o $(ROOTFS)/app/hello ./example/hello

image: build hello
	rm -rf $(IMAGE)
	$(TOOL) build \
		--rootfs $(ROOTFS) \
		--output $(IMAGE) \
		--tag hello-darwin:latest \
		--entrypoint /app/hello \
		--env PATH=/usr/local/bin:/usr/bin:/bin \
		--workdir /

nginx-rootfs:
	sh example/nginx/build-rootfs.sh $(NGINX_ROOTFS) $(NGINX_CONFIG)

nginx: build nginx-rootfs
	rm -rf $(NGINX_IMAGE)
	$(TOOL) build \
		--rootfs $(NGINX_ROOTFS) \
		--output $(NGINX_IMAGE) \
		--tag nginx-darwin:latest \
		--entrypoint /usr/local/bin/nginx \
		--arg=-p \
		--arg=./ \
		--arg=-c \
		--arg=etc/nginx/nginx.conf \
		--env=SSL_CERT_FILE=/etc/ssl/cert.pem \
		--workdir /

nginx-run: build nginx
	$(TOOL) run $(NGINX_IMAGE)

inspect: build image
	$(TOOL) inspect --tag hello-darwin:latest $(IMAGE)

unpack: build image
	rm -rf /tmp/darwin-oci-rootfs
	$(TOOL) unpack --tag hello-darwin:latest --output /tmp/darwin-oci-rootfs $(IMAGE)
	find /tmp/darwin-oci-rootfs -maxdepth 3 -print

run: build image
	$(TOOL) run --arg hello --arg from --arg an --arg OCI --arg layout $(IMAGE)

push: image
	@test -n "$(REGISTRY_IMAGE)" || { echo "set REGISTRY_IMAGE, for example: make push REGISTRY_IMAGE=ghcr.io/OWNER/hello-darwin:latest" >&2; exit 2; }
	$(SKOPEO) copy \
		--format oci \
		--preserve-digests \
		oci:$(IMAGE) \
		docker://$(REGISTRY_IMAGE)

nginx-push: nginx
	@test -n "$(REGISTRY_IMAGE)" || { echo "set REGISTRY_IMAGE, for example: make nginx-push REGISTRY_IMAGE=ghcr.io/OWNER/nginx-darwin:latest" >&2; exit 2; }
	$(SKOPEO) copy \
		--format oci \
		--preserve-digests \
		oci:$(NGINX_IMAGE) \
		docker://$(REGISTRY_IMAGE)

pull: build
	@test -n "$(REGISTRY_IMAGE)" || { echo "set REGISTRY_IMAGE, for example: make pull REGISTRY_IMAGE=ghcr.io/OWNER/hello-darwin:latest" >&2; exit 2; }
	@rm -rf "$(PULL_LAYOUT)"
	$(SKOPEO) --override-os darwin --override-arch $(DARWIN_ARCH) copy \
		--format oci \
		--dest-oci-accept-uncompressed-layers \
		docker://$(REGISTRY_IMAGE) \
		oci:$(PULL_LAYOUT):latest
	@rm -rf "$(IMAGE)"
	@mv "$(PULL_LAYOUT)" "$(IMAGE)"

nginx-pull: build
	@test -n "$(REGISTRY_IMAGE)" || { echo "set REGISTRY_IMAGE, for example: make nginx-pull REGISTRY_IMAGE=ghcr.io/OWNER/nginx-darwin:latest" >&2; exit 2; }
	@rm -rf "$(NGINX_PULL_LAYOUT)"
	$(SKOPEO) --override-os darwin --override-arch $(DARWIN_ARCH) copy \
		--format oci \
		--dest-oci-accept-uncompressed-layers \
		docker://$(REGISTRY_IMAGE) \
		oci:$(NGINX_PULL_LAYOUT):latest
	@rm -rf "$(NGINX_IMAGE)"
	@mv "$(NGINX_PULL_LAYOUT)" "$(NGINX_IMAGE)"

test:
	go test ./...

clean:
	rm -rf bin $(ROOTFS) $(IMAGE) $(PULL_LAYOUT) $(NGINX_ROOTFS) $(NGINX_IMAGE) $(NGINX_PULL_LAYOUT)
