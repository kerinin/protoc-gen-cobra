package main

import (
	"github.com/kerinin/protoc-gen-cobra/example/cmd"
	"github.com/kerinin/protoc-gen-cobra/example/pb"
)

func init() {
	// Add client generated commands to cobra's root cmd.
	cmd.RootCmd.AddCommand(pb.BankClientCommand)
	cmd.RootCmd.AddCommand(pb.CacheClientCommand)
	cmd.RootCmd.AddCommand(pb.TimerClientCommand)
}
