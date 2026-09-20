package main

import "github.com/yhu/fs-hash-util/cmd"

var version = "dev"

func main() {
	cmd.SetVersion(version)
	cmd.Execute()
}
