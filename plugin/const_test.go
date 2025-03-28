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

package plugin

import (
	"fmt"
	"testing"
)

func TestConstToString(t *testing.T) {
	kinds := map[fmt.Stringer]string{
		Audit:                     "Audit",
		Authentication:            "Authentication",
		Schema:                    "Schema",
		Daemon:                    "Daemon",
		Uninitialized:             "Uninitialized",
		Ready:                     "Ready",
		Dying:                     "Dying",
		Disable:                   "Disable",
		Connected:                 "Connected",
		Disconnect:                "Disconnect",
		ChangeUser:                "ChangeUser",
		PreAuth:                   "PreAuth",
		ConnectionEvent(byte(15)): "",
	}
	for key, value := range kinds {
		if key.String() != value {
			t.Errorf("kind %s != %s", key.String(), kinds)
		}
	}
}
