package main

import "github.com/yhu/fs-hash-util/cmd"

var version = "1.0.0"

func main() {
	cmd.SetVersion(version)
	cmd.Execute()
}
