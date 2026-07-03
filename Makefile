.SILENT:

CONTAINER_NAME := ghcr.io/sonalys/anilist-ics
VERSION := $(shell git describe --tags --always --dirty)

build-container:
	docker build --push --build-arg VERSION=$(VERSION) -t $(CONTAINER_NAME):$(VERSION) .