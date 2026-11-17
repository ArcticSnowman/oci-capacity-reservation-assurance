.PHONY: generate
generate:
	@echo "Running $@"
	@go generate ./...


.PHONY: compile
compile:  
	@echo "Running $@"
	@CGO_ENABLED=0 GOBIN=${PWD}/bin go install -v .
	
.PHONY: compress
compress: compile
	@echo "Running $@"
	@upx bin/oci-capacity-reservation-assurance 