package iocodec

import (
	"encoding/xml"
	"io"
	"io/ioutil"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"gopkg.in/yaml.v2"
)

// DefaultDecoders contains the default list of decoders per MIME type.
var DefaultDecoders = DecoderGroup{
	"xml":  DecoderMakerFunc(func(r io.Reader) Decoder { return xml.NewDecoder(r) }),
	"json": DecoderMakerFunc(func(r io.Reader) Decoder { return &jsonDecoder{r} }),
	"yaml": DecoderMakerFunc(func(r io.Reader) Decoder { return &yamlDecoder{r} }),
}

type (
	// A Decoder decodes data into v.
	Decoder interface {
		Decode(v interface{}) error
	}

	// A DecoderGroup maps MIME types to DecoderMakers.
	DecoderGroup map[string]DecoderMaker

	// A DecoderMaker creates and returns a new Decoder.
	DecoderMaker interface {
		NewDecoder(r io.Reader) Decoder
	}

	// DecoderMakerFunc is an adapter for creating DecoderMakers
	// from functions.
	DecoderMakerFunc func(r io.Reader) Decoder
)

// NewDecoder implements the DecoderMaker interface.
func (f DecoderMakerFunc) NewDecoder(r io.Reader) Decoder {
	return f(r)
}

type jsonDecoder struct {
	r           io.Reader
	unmarshaler jsonpb.Unmarshaler
}

func (jd *jsonDecoder) Decode(v interface{}) error {
	return jd.unmarshaler.Unmarshal(jd.r, v.(proto.Message))
}

type yamlDecoder struct {
	r io.Reader
}

func (yd *yamlDecoder) Decode(v interface{}) error {
	b, err := ioutil.ReadAll(yd.r)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}
