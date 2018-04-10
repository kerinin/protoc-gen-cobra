package iocodec

import (
	"encoding/xml"
	"io"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"gopkg.in/yaml.v2"
)

// DefaultEncoders contains the default list of encoders per MIME type.
var DefaultEncoders = EncoderGroup{
	"xml":        EncoderMakerFunc(func(w io.Writer) Encoder { return &xmlEncoder{w} }),
	"json":       EncoderMakerFunc(func(w io.Writer) Encoder { return &jsonEncoder{w: w} }),
	"prettyjson": EncoderMakerFunc(func(w io.Writer) Encoder { return &jsonEncoder{w: w, pretty: true} }),
	"yaml":       EncoderMakerFunc(func(w io.Writer) Encoder { return &yamlEncoder{w} }),
}

type (
	// An Encoder encodes data from v.
	Encoder interface {
		Encode(v interface{}) error
	}

	// An EncoderGroup maps MIME types to EncoderMakers.
	EncoderGroup map[string]EncoderMaker

	// An EncoderMaker creates and returns a new Encoder.
	EncoderMaker interface {
		NewEncoder(w io.Writer) Encoder
	}

	// EncoderMakerFunc is an adapter for creating EncoderMakers
	// from functions.
	EncoderMakerFunc func(w io.Writer) Encoder
)

// NewEncoder implements the EncoderMaker interface.
func (f EncoderMakerFunc) NewEncoder(w io.Writer) Encoder {
	return f(w)
}

type xmlEncoder struct {
	w io.Writer
}

func (xe *xmlEncoder) Encode(v interface{}) error {
	xe.w.Write([]byte(xml.Header))
	defer xe.w.Write([]byte("\n"))
	e := xml.NewEncoder(xe.w)
	e.Indent("", "\t")
	return e.Encode(v)
}

type jsonEncoder struct {
	w         io.Writer
	pretty    bool
	marshaler jsonpb.Marshaler
}

func (je *jsonEncoder) Encode(v interface{}) error {
	if je.pretty {
		je.marshaler.Indent = "\t"
	} else {
		je.marshaler.Indent = ""
	}
	return je.marshaler.Marshal(je.w, v.(proto.Message))
}

type yamlEncoder struct {
	w io.Writer
}

func (ye *yamlEncoder) Encode(v interface{}) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = ye.w.Write(b)
	return err
}
