all:
	protoc \
		-I. \
		--gofast_out=plugins=grpc:. \
		--cobra_out=plugins=client:. \
		*.proto && \
	protoc \
		-I. \
		--gofast_out=plugins=grpc:$(GOPATH)/src \
		--cobra_out=plugins=client:$(GOPATH)/src \
		mismatch/*.proto && \
	protoc \
		-I. \
		--gofast_out=plugins=grpc:$(GOPATH)/src \
		subpackage/*.proto && \
	protoc \
		-I. \
		--gofast_out=plugins=grpc:$(GOPATH)/src \
		subpackage/subsubpackage/*.proto

clean:
	rm -f *.pb.go

deps:
	go get github.com/gogo/protobuf/protoc-gen-gofast
	go get github.com/kerinin/protoc-gen-cobra
