package main

import "github.com/yhu/fs-hash-util/cmd"

var version = "1.4.2"

func main() {
	cmd.SetVersion(version)
	cmd.Execute()
}
