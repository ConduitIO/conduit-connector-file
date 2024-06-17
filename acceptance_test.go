// Copyright © 2022 Meroxa, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package file

import (
	"fmt"
	"os"
	"testing"
	"time"

	sdk "github.com/conduitio/conduit-connector-sdk"
	"go.uber.org/goleak"
)

func TestAcceptance(t *testing.T) {
	testFile := fmt.Sprintf("%v/acceptance-test-%d.txt", t.TempDir(), time.Now().UnixMicro()%1000)

	t.Logf("using test file %v in acceptance tests", testFile)
	sdk.AcceptanceTest(t, sdk.ConfigurableAcceptanceTestDriver{
		Config: sdk.ConfigurableAcceptanceTestDriverConfig{
			Connector: Connector,
			SourceConfig: map[string]string{
				"path": testFile,
			},
			DestinationConfig: map[string]string{
				"path": testFile,
			},

			AfterTest: func(t *testing.T) {
				err := os.Remove(testFile)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			},

			GoleakOptions: []goleak.Option{
				// tail will initialize a InotifyTracker and keep it running
				// forever as a shared instance, both goroutines belong to it
				goleak.IgnoreTopFunction("github.com/nxadm/tail/watch.(*InotifyTracker).run"),
				goleak.IgnoreAnyFunction("github.com/fsnotify/fsnotify.(*Watcher).readEvents"),
				goleak.IgnoreTopFunction("syscall.Syscall6"), // linux
				goleak.IgnoreTopFunction("syscall.syscall6"), // darwin
			},
			ReadTimeout:  time.Second,
			WriteTimeout: time.Second,
		},
	})
}
