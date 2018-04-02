package client

import (
	"regexp"
	"strings"

	"github.com/fiorix/protoc-gen-cobra/generator"
)

var matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
var matchAllCap = regexp.MustCompile("([a-z0-9])([A-Z])")

func (c *client) name(s string) string {
	switch c.gen.Namer {
	case "underscored":
		return underscoredNamer(s)
	case "dashed":
		return dashedNamer(s)
	default:
		return lowerCamelNamer(s)
	}
}

func lowerCamelNamer(s string) string {
	return strings.ToLower(generator.CamelCase(s))
}

func underscoredNamer(s string) string {
	snake := matchFirstCap.ReplaceAllString(s, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	return strings.ToLower(snake)
}

func dashedNamer(s string) string {
	snake := matchFirstCap.ReplaceAllString(s, "${1}-${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}-${2}")
	return strings.ToLower(snake)
}
