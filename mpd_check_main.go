package main

import (
	"fmt"
	"time"

	"github.com/prometheus/common/model"
)

func main() {
	for _, s := range []string{"0s", "0", "2w", "90d", "1y", "2160h", "36h", "1w2d", "2d1w"} {
		d, err := model.ParseDuration(s)
		if err != nil {
			fmt.Printf("%-8q ERROR: %v\n", s, err)
			continue
		}
		fmt.Printf("%-8q = %v (%d days)\n", s, time.Duration(d), int64(time.Duration(d)/(24*time.Hour)))
	}
}
