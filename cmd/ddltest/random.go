// Copyright 2025 Ekjot Singh
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package ddltest

import (
	"math/rand"
	"strconv"
)

func randomInt() int {
	return rand.Int()
}

func randomIntn(n int) int {
	return rand.Intn(n)
}

func randomFloat() float64 {
	return rand.Float64()
}

func randomString(n int) string {
	const alphanum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var bytes = make([]byte, n)
	for i := range bytes {
		bytes[i] = alphanum[randomIntn(len(alphanum))]
	}
	return string(bytes)
}

// Args
// 0 -> min
// 1 -> max
// randomNum(1,10) -> [1,10)
// randomNum(-1) -> random
// randomNum() -> random
func randomNum(args ...int) int {
	if len(args) > 1 {
		return args[0] + randomIntn(args[1]-args[0])
	} else if len(args) == 1 {
		return randomIntn(args[0])
	} else {
		return randomInt()
	}
}

// Args
// 0 -> min
// 1 -> max
// 2 -> prec
// randomFloat64(1,10) -> [1.0,10.0]
// randomFloat64(1,10,3) -> [1.000,10.000]
// randomFloat64(-1) -> random
// randomFloat64() -> random
func randomFloat64(args ...int) float64 {
	value := float64(randomNum(args...))

	if len(args) > 2 {
		fvalue := strconv.FormatFloat(value, 'f', args[2], 64)
		value, _ = strconv.ParseFloat(fvalue, 64)
	}

	return value
}

// true/false
func randomBool() bool {
	value := randomIntn(2)
	return value == 1
}
