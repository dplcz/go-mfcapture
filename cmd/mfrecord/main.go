package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"mfcapture"
)

func main() {
	duration := flag.Duration("duration", 5*time.Second, "recording duration")
	output := flag.String("o", "out.mp4", "output mp4 path")
	bitrate := flag.Uint("bitrate", 12_000_000, "bitrate in bits per second")
	monitor := flag.Uint("monitor", 0, "monitor index")
	maxFPS := flag.Uint("maxfps", 0, "cap on frames written per second (0 = unlimited)")
	flag.Parse()

	cfg := mfcapture.Config{
		OutputPath: *output,
		Duration:   *duration,
		BitrateBps: uint32(*bitrate),
		Output:     *monitor,
		MaxFPS:     uint32(*maxFPS),
	}

	if err := mfcapture.Record(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "record failed:", err)
		os.Exit(1)
	}
	fmt.Println("done:", *output, "duration:", *duration)
}
