package main

import (
	"math"
	"math/rand/v2"
	"time"
)

func jitteredDelay(base time.Duration, random float64) time.Duration {
	factor := 0.8 + random*0.4
	return max(0, time.Duration(math.Round(float64(base)*factor)))
}

func randomJitteredDelay(base time.Duration) time.Duration {
	return jitteredDelay(base, rand.Float64())
}
