FROM golang:latest

RUN apt-get update
RUN apt-get install -y unzip wget

RUN wget https://github.com/google/protobuf/releases/download/v3.5.1/protoc-3.5.1-linux-x86_64.zip && \
      unzip *.zip && \
      mv bin/protoc /usr/local/bin && \
      mv include/google /usr/local/include && \
      rm -rf include readme.txt

RUN pwd
RUN ls -l

WORKDIR /go/src/github.com/fiorix/protoc-gen-cobra
COPY vendor /go/src/github.com/fiorix/protoc-gen-cobra/vendor

RUN cd vendor/github.com/golang/protobuf/protoc-gen-go && go install
RUN cd vendor/github.com/gogo/protobuf/protoc-gen-gofast && go install

COPY . /go/src/github.com/fiorix/protoc-gen-cobra

RUN go install
RUN cd example/pb && make
