package main

import (
	"fmt"
)

func DebugLog(v ...interface{}) {
	if debug {
		data := fmt.Sprintln(v...)
		fmt.Println(data)
		// TODO: could also write to file
	}
}
