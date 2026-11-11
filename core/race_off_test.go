//go:build !race

package core

// raceOn 测试是不是在 -race 下跑(见 anytlsRaceSkip)。
const raceOn = false
