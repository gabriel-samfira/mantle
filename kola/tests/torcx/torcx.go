// Copyright 2017 CoreOS, Inc.
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

package torcx

import (
	"github.com/flatcar/mantle/kola/cluster"
	"github.com/flatcar/mantle/kola/register"
	"github.com/flatcar/mantle/platform/conf"
)

func init() {
	// Regression test for https://github.com/coreos/bugs/issues/2079
	// Note: it would be preferable to not conflate docker + torcx in this
	// testing, but rather to use a standalone torcx package/profile
	register.Register(&register.Test{
		Run:         torcxEnable,
		ClusterSize: 1,
		// This test is normally not related to the cloud environment
		Platforms: []string{"qemu", "qemu-unpriv"},
		Name:      "torcx.enable-service",
		UserData: conf.ContainerLinuxConfig(`
systemd:
  units:
  - name: docker.service
    enable: true
`),
		Distros: []string{"cl"},
	})
}

func torcxEnable(c cluster.TestCluster) {
	m := c.Machines()[0]
	output := c.MustSSH(m, `systemctl is-enabled docker`)
	if string(output) != "enabled" {
		c.Errorf("expected enabled, got %v", output)
	}
}
