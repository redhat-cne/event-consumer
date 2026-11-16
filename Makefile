IMG ?= quay.io/redhat-cne/cloud-event-consumer:latest
CONSUMER_IMG ?= $(IMG)
SECURED ?= true
DIR_CERTS ?= /tmp/certs
CERT_MANAGER ?= false
KUSTOMIZE ?= kustomize
OVERLAY = manifests
ifeq ($(SECURED),true)
OVERLAY = manifests/secured
endif

.PHONY: build test docker-build docker-push deploy-consumer deploy-consumer-secured undeploy-consumer
build:
	CGO_ENABLED=0 go build -o build/cloud-event-consumer ./cmd/cloud-event-consumer
test:
	go test -tags unittests ./...
docker-build:
	docker build -t $(CONSUMER_IMG) .
docker-push:
	docker push $(CONSUMER_IMG)
deploy-consumer:
	cd $(OVERLAY) && $(KUSTOMIZE) edit set image cloud-event-consumer=$(CONSUMER_IMG)
	$(KUSTOMIZE) build $(OVERLAY) | kubectl apply -f -
ifeq ($(SECURED),true)
	DIR_CERTS=$(DIR_CERTS) CERT_MANAGER=$(CERT_MANAGER) bash manifests/auth/setup-secrets.sh
endif
deploy-consumer-secured:
	$(MAKE) deploy-consumer SECURED=true

undeploy-consumer:
	$(KUSTOMIZE) build $(OVERLAY) | kubectl delete --ignore-not-found -f -
