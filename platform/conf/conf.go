// Copyright 2016-2018 CoreOS, Inc.
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

package conf

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"reflect"
	"strings"

	butane "github.com/coreos/butane/config"
	"github.com/coreos/butane/config/common"
	cci "github.com/coreos/coreos-cloudinit/config"
	ign3err "github.com/coreos/ignition/v2/config/shared/errors"
	v3 "github.com/coreos/ignition/v2/config/v3_0"
	v3types "github.com/coreos/ignition/v2/config/v3_0/types"
	v31 "github.com/coreos/ignition/v2/config/v3_1"
	v31types "github.com/coreos/ignition/v2/config/v3_1/types"
	v32 "github.com/coreos/ignition/v2/config/v3_2"
	v32types "github.com/coreos/ignition/v2/config/v3_2/types"
	v33 "github.com/coreos/ignition/v2/config/v3_3"
	v33types "github.com/coreos/ignition/v2/config/v3_3/types"
	ign3validate "github.com/coreos/ignition/v2/config/validate"
	"github.com/coreos/pkg/capnslog"
	ct "github.com/flatcar/container-linux-config-transpiler/config"
	ignerr "github.com/flatcar/ignition/config/shared/errors"
	v1 "github.com/flatcar/ignition/config/v1"
	v1types "github.com/flatcar/ignition/config/v1/types"
	v2 "github.com/flatcar/ignition/config/v2_0"
	v2types "github.com/flatcar/ignition/config/v2_0/types"
	v21 "github.com/flatcar/ignition/config/v2_1"
	v21types "github.com/flatcar/ignition/config/v2_1/types"
	v22 "github.com/flatcar/ignition/config/v2_2"
	v22types "github.com/flatcar/ignition/config/v2_2/types"
	v23 "github.com/flatcar/ignition/config/v2_3"
	v23types "github.com/flatcar/ignition/config/v2_3/types"
	ignvalidate "github.com/flatcar/ignition/config/validate"
	"github.com/vincent-petithory/dataurl"
	"golang.org/x/crypto/ssh/agent"
)

type kind int

const (
	kindEmpty kind = iota
	kindCloudConfig
	kindIgnition
	kindContainerLinuxConfig
	kindScript
	kindButane
	kindMultipartMime
)

var plog = capnslog.NewPackageLogger("github.com/flatcar/mantle", "platform/conf")

// UserData is an immutable, unvalidated configuration for a Container Linux
// machine.
type UserData struct {
	kind      kind
	data      string
	extraKeys []*agent.Key // SSH keys to be injected during rendering
	// user to create.
	User string
}

// Conf is a configuration for a Container Linux machine. It may be either a
// coreos-cloudconfig or an ignition configuration.
type Conf struct {
	ignitionV1    *v1types.Config
	ignitionV2    *v2types.Config
	ignitionV21   *v21types.Config
	ignitionV22   *v22types.Config
	ignitionV23   *v23types.Config
	ignitionV3    *v3types.Config
	ignitionV31   *v31types.Config
	ignitionV32   *v32types.Config
	ignitionV33   *v33types.Config
	cloudconfig   *cci.CloudConfig
	script        string
	multipartMime string
	user          string
}

func Empty() *UserData {
	return &UserData{
		kind: kindEmpty,
	}
}

func MultipartMimeConfig(data string) *UserData {
	return &UserData{
		kind: kindMultipartMime,
		data: data,
	}
}

func ContainerLinuxConfig(data string) *UserData {
	return &UserData{
		kind: kindContainerLinuxConfig,
		data: data,
	}
}

func Ignition(data string) *UserData {
	return &UserData{
		kind: kindIgnition,
		data: data,
	}
}

func CloudConfig(data string) *UserData {
	return &UserData{
		kind: kindCloudConfig,
		data: data,
	}
}

func Butane(data string) *UserData {
	return &UserData{
		kind: kindButane,
		data: data,
	}
}

func Script(data string) *UserData {
	return &UserData{
		kind: kindScript,
		data: data,
	}
}

func Unknown(data string) *UserData {
	u := &UserData{
		data: data,
	}

	_, _, err := v22.Parse([]byte(data))
	switch err {
	case ignerr.ErrEmpty:
		u.kind = kindEmpty
	case ignerr.ErrCloudConfig:
		u.kind = kindCloudConfig
	case ignerr.ErrScript:
		u.kind = kindScript
	case ignerr.ErrMultipartMime:
		u.kind = kindMultipartMime
	default:
		// Guess whether this is an Ignition config or a CLC.
		// This treats an invalid Ignition config as a CLC, and a
		// CLC in the JSON subset of YAML as an Ignition config.
		var decoded interface{}
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			u.kind = kindContainerLinuxConfig
		} else {
			u.kind = kindIgnition
		}
	}

	return u
}

// Contains returns true if the UserData contains the specified string.
func (u *UserData) Contains(substr string) bool {
	return strings.Contains(u.data, substr)
}

// Performs a string substitution and returns a new UserData.
func (u *UserData) Subst(old, new string) *UserData {
	ret := *u
	ret.data = strings.Replace(u.data, old, new, -1)
	return &ret
}

// Adds an SSH key and returns a new UserData.
func (u *UserData) AddKey(key agent.Key) *UserData {
	ret := *u
	ret.extraKeys = append(ret.extraKeys, &key)
	return &ret
}

func (u *UserData) IsIgnitionCompatible() bool {
	return u.kind == kindIgnition || u.kind == kindContainerLinuxConfig || u.kind == kindButane
}

// Render parses userdata and returns a new Conf. It returns an error if the
// userdata can't be parsed.
func (u *UserData) Render(ctPlatform string) (*Conf, error) {
	c := &Conf{user: u.User}

	renderIgnition := func() error {
		// Try each known version in turn.  Newer parsers will
		// fall back to older ones, so try older versions first.
		ignc1, report, err := v1.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV1 = &ignc1
			return nil
		} else if err != ignerr.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report)
			return err
		}

		ignc2, report, err := v2.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV2 = &ignc2
			return nil
		} else if err != ignerr.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report)
			return err
		}

		ignc21, report, err := v21.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV21 = &ignc21
			return nil
		} else if err != ignerr.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report)
			return err
		}

		ignc22, report, err := v22.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV22 = &ignc22
			return nil
		} else if err != ignerr.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report)
			return err
		}

		ignc23, report, err := v23.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV23 = &ignc23
			return nil
		} else if err != ignerr.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report)
			return err
		}

		ignc3, report3, err := v3.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV3 = &ignc3
			return nil
		} else if err != ign3err.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report3)
			return err
		}

		ignc31, report31, err := v31.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV31 = &ignc31
			return nil
		} else if err != ign3err.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report31)
			return err
		}

		ignc32, report32, err := v32.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV32 = &ignc32
			return nil
		} else if err != ign3err.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report32)
			return err
		}

		ignc33, report33, err := v33.Parse([]byte(u.data))
		if err == nil {
			c.ignitionV33 = &ignc33
			return nil
		} else if err != ign3err.ErrUnknownVersion {
			plog.Errorf("invalid userdata: %v", report33)
			return err
		}

		// give up
		return err
	}

	switch u.kind {
	case kindEmpty:
		// empty, noop
	case kindCloudConfig:
		var err error
		c.cloudconfig, err = cci.NewCloudConfig(u.data)
		if err != nil {
			return nil, err
		}
	case kindScript:
		// pass through scripts unmodified, you are on your own.
		c.script = u.data
	case kindMultipartMime:
		c.multipartMime = u.data
	case kindIgnition:
		err := renderIgnition()
		if err != nil {
			return nil, err
		}
	case kindContainerLinuxConfig:
		clc, ast, report := ct.Parse([]byte(u.data))
		if report.IsFatal() {
			return nil, fmt.Errorf("parsing Container Linux config: %s", report)
		} else if len(report.Entries) > 0 {
			plog.Warningf("parsing Container Linux config: %s", report)
		}

		ignc, report := ct.Convert(clc, ctPlatform, ast)
		if report.IsFatal() {
			return nil, fmt.Errorf("rendering Container Linux config for platform %q: %s", ctPlatform, report)
		} else if len(report.Entries) > 0 {
			plog.Warningf("rendering Container Linux config: %s", report)
		}

		c.ignitionV23 = &ignc
	case kindButane:
		// CLC translation is done in two steps:
		// * Parsing the data
		// * Converting the CLC parsed data to Ignition types (bound to the Ignition spec version)
		// Butane is a bit different, so we convert data directly to Ignition3.3.0 bytes, butane will
		// take care itself to parse the variant / version of the config to do the right translation with an Ignition
		// version >= 3.3.0
		ignc, report, err := butane.TranslateBytes([]byte(u.data), common.TranslateBytesOptions{})
		if err != nil {
			return nil, fmt.Errorf("converting Butane to Ignition: %w", err)
		}

		if report.IsFatal() {
			return nil, fmt.Errorf("converting Butane to Ignition: %s", report.String())
		}

		if len(report.Entries) > 0 {
			plog.Warningf("parsing Butane config: %s", report.String())
		}

		// Doing that allows to benefit from existing Ignition mechanism:
		// CopyKeys, AddSystemdUnit, etc.
		u.data = string(ignc)

		// for consistency.
		u.kind = kindIgnition

		// Config is now considered as an Ignition configuration.
		if err := renderIgnition(); err != nil {
			return nil, err
		}
	default:
		panic("invalid kind")
	}

	if len(u.extraKeys) > 0 {
		// not a no-op in the zero-key case
		c.CopyKeys(u.extraKeys)
	}

	return c, nil
}

// String returns the string representation of the userdata in Conf.
func (c *Conf) String() string {
	if c.ignitionV1 != nil {
		buf, _ := json.Marshal(c.ignitionV1)
		return string(buf)
	} else if c.ignitionV2 != nil {
		buf, _ := json.Marshal(c.ignitionV2)
		return string(buf)
	} else if c.ignitionV21 != nil {
		buf, _ := json.Marshal(c.ignitionV21)
		return string(buf)
	} else if c.ignitionV22 != nil {
		buf, _ := json.Marshal(c.ignitionV22)
		return string(buf)
	} else if c.ignitionV23 != nil {
		buf, _ := json.Marshal(c.ignitionV23)
		return string(buf)
	} else if c.ignitionV3 != nil {
		buf, _ := json.Marshal(c.ignitionV3)
		return string(buf)
	} else if c.ignitionV31 != nil {
		buf, _ := json.Marshal(c.ignitionV31)
		return string(buf)
	} else if c.ignitionV32 != nil {
		buf, _ := json.Marshal(c.ignitionV32)
		return string(buf)
	} else if c.ignitionV33 != nil {
		buf, _ := json.Marshal(c.ignitionV33)
		return string(buf)
	} else if c.cloudconfig != nil {
		return c.cloudconfig.String()
	} else if c.script != "" {
		return c.script
	} else if c.multipartMime != "" {
		return c.multipartMime
	}

	return ""
}

// MergeV3 merges a config with the ignitionV3 config via Ignition's merging function.
func (c *Conf) MergeV3(newConfig v3types.Config) {
	mergeConfig := v3.Merge(*c.ignitionV3, newConfig)
	c.ignitionV3 = &mergeConfig
}

func (c *Conf) MergeV31(newConfig v31types.Config) {
	mergeConfig := v31.Merge(*c.ignitionV31, newConfig)
	c.ignitionV31 = &mergeConfig
}

func (c *Conf) MergeV32(newConfig v32types.Config) {
	mergeConfig := v32.Merge(*c.ignitionV32, newConfig)
	c.ignitionV32 = &mergeConfig
}

func (c *Conf) MergeV33(newConfig v33types.Config) {
	mergeConfig := v33.Merge(*c.ignitionV33, newConfig)
	c.ignitionV33 = &mergeConfig
}

func (c *Conf) ValidConfig() bool {
	if !c.IsIgnition() {
		return false
	}
	val := c.getIgnitionValidateValue()
	if c.ignitionV3 != nil {
		rpt := ign3validate.ValidateWithContext(c.ignitionV3, nil)
		return !rpt.IsFatal()
	} else {
		rpt := ignvalidate.ValidateWithoutSource(val)
		return !rpt.IsFatal()
	}
}

func (c *Conf) getIgnitionValidateValue() reflect.Value {
	if c.ignitionV1 != nil {
		return reflect.ValueOf(c.ignitionV1)
	} else if c.ignitionV2 != nil {
		return reflect.ValueOf(c.ignitionV2)
	} else if c.ignitionV21 != nil {
		return reflect.ValueOf(c.ignitionV21)
	} else if c.ignitionV22 != nil {
		return reflect.ValueOf(c.ignitionV22)
	} else if c.ignitionV23 != nil {
		return reflect.ValueOf(c.ignitionV23)
	} else if c.ignitionV3 != nil {
		return reflect.ValueOf(c.ignitionV3)
	} else if c.ignitionV31 != nil {
		return reflect.ValueOf(c.ignitionV31)
	} else if c.ignitionV32 != nil {
		return reflect.ValueOf(c.ignitionV32)
	} else if c.ignitionV33 != nil {
		return reflect.ValueOf(c.ignitionV33)
	}
	return reflect.ValueOf(nil)
}

// WriteFile writes the userdata in Conf to a local file.
func (c *Conf) WriteFile(name string) error {
	return ioutil.WriteFile(name, []byte(c.String()), 0666)
}

// Bytes returns the serialized userdata in Conf.
func (c *Conf) Bytes() []byte {
	return []byte(c.String())
}

func (c *Conf) addFileV2(path, filesystem, contents string, mode int) {
	u, err := url.Parse(dataurl.EncodeBytes([]byte(contents)))
	if err != nil {
		plog.Warningf("parsing dataurl contents: %v", err)
		return
	}
	c.ignitionV2.Storage.Files = append(c.ignitionV2.Storage.Files, v2types.File{
		Filesystem: filesystem,
		Path:       v2types.Path(path),
		Contents: v2types.FileContents{
			Source: v2types.Url(*u),
		},
		Mode: v2types.FileMode(os.FileMode(mode)),
	})
}

func (c *Conf) addFileV21(path, filesystem, contents string, mode int) {
	c.ignitionV21.Storage.Files = append(c.ignitionV21.Storage.Files, v21types.File{
		Node: v21types.Node{
			Filesystem: filesystem,
			Path:       path,
		},
		FileEmbedded1: v21types.FileEmbedded1{
			Contents: v21types.FileContents{
				Source: dataurl.EncodeBytes([]byte(contents)),
			},
			Mode: mode,
		},
	})
}

func (c *Conf) addFileV22(path, filesystem, contents string, mode int) {
	c.ignitionV22.Storage.Files = append(c.ignitionV22.Storage.Files, v22types.File{
		Node: v22types.Node{
			Filesystem: filesystem,
			Path:       path,
		},
		FileEmbedded1: v22types.FileEmbedded1{
			Contents: v22types.FileContents{
				Source: dataurl.EncodeBytes([]byte(contents)),
			},
			Mode: &mode,
		},
	})
}

func (c *Conf) addFileV23(path, filesystem, contents string, mode int) {
	c.ignitionV23.Storage.Files = append(c.ignitionV23.Storage.Files, v23types.File{
		Node: v23types.Node{
			Filesystem: filesystem,
			Path:       path,
		},
		FileEmbedded1: v23types.FileEmbedded1{
			Contents: v23types.FileContents{
				Source: dataurl.EncodeBytes([]byte(contents)),
			},
			Mode: &mode,
		},
	})
}

func (c *Conf) addFileV3(path, filesystem, contents string, mode int) {
	source := dataurl.EncodeBytes([]byte(contents))
	newConfig := v3types.Config{
		Ignition: v3types.Ignition{
			Version: "3.0.0",
		},
		Storage: v3types.Storage{
			Files: []v3types.File{
				{
					Node: v3types.Node{
						Path: path,
					},
					FileEmbedded1: v3types.FileEmbedded1{
						Contents: v3types.FileContents{
							Source: &source,
						},
						Mode: &mode,
					},
				},
			},
		},
	}
	c.MergeV3(newConfig)
}

func (c *Conf) addFileV31(path, filesystem, contents string, mode int) {
	source := dataurl.EncodeBytes([]byte(contents))
	newConfig := v31types.Config{
		Ignition: v31types.Ignition{
			Version: "3.1.0",
		},
		Storage: v31types.Storage{
			Files: []v31types.File{
				{
					Node: v31types.Node{
						Path: path,
					},
					FileEmbedded1: v31types.FileEmbedded1{
						Contents: v31types.Resource{
							Source: &source,
						},
						Mode: &mode,
					},
				},
			},
		},
	}
	c.MergeV31(newConfig)
}

func (c *Conf) addFileV32(path, filesystem, contents string, mode int) {
	source := dataurl.EncodeBytes([]byte(contents))
	newConfig := v32types.Config{
		Ignition: v32types.Ignition{
			Version: "3.2.0",
		},
		Storage: v32types.Storage{
			Files: []v32types.File{
				{
					Node: v32types.Node{
						Path: path,
					},
					FileEmbedded1: v32types.FileEmbedded1{
						Contents: v32types.Resource{
							Source: &source,
						},
						Mode: &mode,
					},
				},
			},
		},
	}
	c.MergeV32(newConfig)
}

func (c *Conf) addFileV33(path, filesystem, contents string, mode int) {
	source := dataurl.EncodeBytes([]byte(contents))
	newConfig := v33types.Config{
		Ignition: v33types.Ignition{
			Version: "3.3.0",
		},
		Storage: v33types.Storage{
			Files: []v33types.File{
				{
					Node: v33types.Node{
						Path: path,
					},
					FileEmbedded1: v33types.FileEmbedded1{
						Contents: v33types.Resource{
							Source: &source,
						},
						Mode: &mode,
					},
				},
			},
		},
	}
	c.MergeV33(newConfig)
}
func (c *Conf) addFileV1(path, filesystem, contents string, mode int) {
	file := v1types.File{
		Path:     v1types.Path(path),
		Contents: contents,
		Mode:     v1types.FileMode(os.FileMode(mode)),
	}
	if len(c.ignitionV1.Storage.Filesystems) == 0 {
		c.ignitionV1.Storage.Filesystems = []v1types.Filesystem{
			v1types.Filesystem{
				Files:  []v1types.File{file},
				Create: nil,
				Format: "ext4",
				Device: "/dev/disk/by-partlabel/ROOT",
			},
		}
	} else {
		if c.ignitionV1.Storage.Filesystems[0].Device != "/dev/disk/by-partlabel/ROOT" {
			panic(fmt.Errorf("test specified an unexpected filesystem with Ignition v1: %q", c.ignitionV1.Storage.Filesystems[0].Device))
		}
		c.ignitionV1.Storage.Filesystems[0].Files = append(c.ignitionV1.Storage.Filesystems[0].Files, file)

	}
}

func (c *Conf) addFileCloudConfig(path, filesystem, contents string, mode int) {
	c.cloudconfig.WriteFiles = append(c.cloudconfig.WriteFiles, cci.File{
		Content:            contents,
		Owner:              "root",
		Path:               path,
		RawFilePermissions: fmt.Sprintf("%#o", mode),
	})
}

func (c *Conf) AddFile(path, filesystem, contents string, mode int) {
	if c.ignitionV33 != nil {
		c.addFileV33(path, filesystem, contents, mode)
	} else if c.ignitionV32 != nil {
		c.addFileV32(path, filesystem, contents, mode)
	} else if c.ignitionV31 != nil {
		c.addFileV31(path, filesystem, contents, mode)
	} else if c.ignitionV3 != nil {
		c.addFileV3(path, filesystem, contents, mode)
	} else if c.ignitionV2 != nil {
		c.addFileV2(path, filesystem, contents, mode)
	} else if c.ignitionV21 != nil {
		c.addFileV21(path, filesystem, contents, mode)
	} else if c.ignitionV22 != nil {
		c.addFileV22(path, filesystem, contents, mode)
	} else if c.ignitionV23 != nil {
		c.addFileV23(path, filesystem, contents, mode)
	} else if c.ignitionV1 != nil {
		c.addFileV1(path, filesystem, contents, mode)
	} else if c.cloudconfig != nil {
		c.addFileCloudConfig(path, filesystem, contents, mode)
	} else {
		panic(fmt.Errorf("unimplemented case in AddFile"))
	}
}

func (c *Conf) addSystemdUnitV1(name, contents string, enable bool) {
	c.ignitionV1.Systemd.Units = append(c.ignitionV1.Systemd.Units, v1types.SystemdUnit{
		Name:     v1types.SystemdUnitName(name),
		Contents: contents,
		Enable:   enable,
	})
}

func (c *Conf) addSystemdUnitV2(name, contents string, enable bool) {
	c.ignitionV2.Systemd.Units = append(c.ignitionV2.Systemd.Units, v2types.SystemdUnit{
		Name:     v2types.SystemdUnitName(name),
		Contents: contents,
		Enable:   enable,
	})
}

func (c *Conf) addSystemdUnitV21(name, contents string, enable bool) {
	c.ignitionV21.Systemd.Units = append(c.ignitionV21.Systemd.Units, v21types.Unit{
		Name:     name,
		Contents: contents,
		Enabled:  &enable,
	})
}

func (c *Conf) addSystemdUnitV22(name, contents string, enable bool) {
	c.ignitionV22.Systemd.Units = append(c.ignitionV22.Systemd.Units, v22types.Unit{
		Name:     name,
		Contents: contents,
		Enabled:  &enable,
	})
}

func (c *Conf) addSystemdUnitV23(name, contents string, enable bool) {
	c.ignitionV23.Systemd.Units = append(c.ignitionV23.Systemd.Units, v23types.Unit{
		Name:     name,
		Contents: contents,
		Enabled:  &enable,
	})
}

func (c *Conf) addSystemdUnitV3(name, contents string, enable bool) {
	newConfig := v3types.Config{
		Ignition: v3types.Ignition{
			Version: "3.0.0",
		},
		Systemd: v3types.Systemd{
			Units: []v3types.Unit{
				{
					Name:     name,
					Contents: &contents,
					Enabled:  &enable,
				},
			},
		},
	}
	c.MergeV3(newConfig)
}

func (c *Conf) addSystemdUnitV31(name, contents string, enable bool) {
	newConfig := v31types.Config{
		Ignition: v31types.Ignition{
			Version: "3.1.0",
		},
		Systemd: v31types.Systemd{
			Units: []v31types.Unit{
				{
					Name:     name,
					Contents: &contents,
					Enabled:  &enable,
				},
			},
		},
	}
	c.MergeV31(newConfig)
}

func (c *Conf) addSystemdUnitV32(name, contents string, enable bool) {
	newConfig := v32types.Config{
		Ignition: v32types.Ignition{
			Version: "3.2.0",
		},
		Systemd: v32types.Systemd{
			Units: []v32types.Unit{
				{
					Name:     name,
					Contents: &contents,
					Enabled:  &enable,
				},
			},
		},
	}
	c.MergeV32(newConfig)
}

func (c *Conf) addSystemdUnitV33(name, contents string, enable bool) {
	newConfig := v33types.Config{
		Ignition: v33types.Ignition{
			Version: "3.3.0",
		},
		Systemd: v33types.Systemd{
			Units: []v33types.Unit{
				{
					Name:     name,
					Contents: &contents,
					Enabled:  &enable,
				},
			},
		},
	}
	c.MergeV33(newConfig)
}

func (c *Conf) addSystemdUnitCloudConfig(name, contents string, enable bool) {
	c.cloudconfig.CoreOS.Units = append(c.cloudconfig.CoreOS.Units, cci.Unit{
		Name:    name,
		Content: contents,
		Enable:  enable,
	})
}

func (c *Conf) AddSystemdUnit(name, contents string, enable bool) {
	if c.ignitionV1 != nil {
		c.addSystemdUnitV1(name, contents, enable)
	} else if c.ignitionV2 != nil {
		c.addSystemdUnitV2(name, contents, enable)
	} else if c.ignitionV21 != nil {
		c.addSystemdUnitV21(name, contents, enable)
	} else if c.ignitionV22 != nil {
		c.addSystemdUnitV22(name, contents, enable)
	} else if c.ignitionV23 != nil {
		c.addSystemdUnitV23(name, contents, enable)
	} else if c.ignitionV3 != nil {
		c.addSystemdUnitV3(name, contents, enable)
	} else if c.ignitionV31 != nil {
		c.addSystemdUnitV31(name, contents, enable)
	} else if c.ignitionV32 != nil {
		c.addSystemdUnitV32(name, contents, enable)
	} else if c.ignitionV33 != nil {
		c.addSystemdUnitV33(name, contents, enable)
	} else if c.cloudconfig != nil {
		c.addSystemdUnitCloudConfig(name, contents, enable)
	}
}

func (c *Conf) addSystemdDropinV1(service, name, contents string) {
	for i, unit := range c.ignitionV1.Systemd.Units {
		if unit.Name == v1types.SystemdUnitName(service) {
			unit.DropIns = append(unit.DropIns, v1types.SystemdUnitDropIn{
				Name:     v1types.SystemdUnitDropInName(name),
				Contents: contents,
			})
			c.ignitionV1.Systemd.Units[i] = unit
			return
		}
	}
	c.ignitionV1.Systemd.Units = append(c.ignitionV1.Systemd.Units, v1types.SystemdUnit{
		Name: v1types.SystemdUnitName(service),
		DropIns: []v1types.SystemdUnitDropIn{
			{
				Name:     v1types.SystemdUnitDropInName(name),
				Contents: contents,
			},
		},
	})
}

func (c *Conf) addSystemdDropinV2(service, name, contents string) {
	for i, unit := range c.ignitionV2.Systemd.Units {
		if unit.Name == v2types.SystemdUnitName(service) {
			unit.DropIns = append(unit.DropIns, v2types.SystemdUnitDropIn{
				Name:     v2types.SystemdUnitDropInName(name),
				Contents: contents,
			})
			c.ignitionV2.Systemd.Units[i] = unit
			return
		}
	}
	c.ignitionV2.Systemd.Units = append(c.ignitionV2.Systemd.Units, v2types.SystemdUnit{
		Name: v2types.SystemdUnitName(service),
		DropIns: []v2types.SystemdUnitDropIn{
			{
				Name:     v2types.SystemdUnitDropInName(name),
				Contents: contents,
			},
		},
	})
}

func (c *Conf) addSystemdDropinV21(service, name, contents string) {
	for i, unit := range c.ignitionV21.Systemd.Units {
		if unit.Name == service {
			unit.Dropins = append(unit.Dropins, v21types.Dropin{
				Name:     name,
				Contents: contents,
			})
			c.ignitionV21.Systemd.Units[i] = unit
			return
		}
	}
	c.ignitionV21.Systemd.Units = append(c.ignitionV21.Systemd.Units, v21types.Unit{
		Name: service,
		Dropins: []v21types.Dropin{
			{
				Name:     name,
				Contents: contents,
			},
		},
	})
}

func (c *Conf) addSystemdDropinV22(service, name, contents string) {
	for i, unit := range c.ignitionV22.Systemd.Units {
		if unit.Name == service {
			unit.Dropins = append(unit.Dropins, v22types.SystemdDropin{
				Name:     name,
				Contents: contents,
			})
			c.ignitionV22.Systemd.Units[i] = unit
			return
		}
	}
	c.ignitionV22.Systemd.Units = append(c.ignitionV22.Systemd.Units, v22types.Unit{
		Name: service,
		Dropins: []v22types.SystemdDropin{
			{
				Name:     name,
				Contents: contents,
			},
		},
	})
}

func (c *Conf) addSystemdDropinV23(service, name, contents string) {
	for i, unit := range c.ignitionV23.Systemd.Units {
		if unit.Name == service {
			unit.Dropins = append(unit.Dropins, v23types.SystemdDropin{
				Name:     name,
				Contents: contents,
			})
			c.ignitionV23.Systemd.Units[i] = unit
			return
		}
	}
	c.ignitionV23.Systemd.Units = append(c.ignitionV23.Systemd.Units, v23types.Unit{
		Name: service,
		Dropins: []v23types.SystemdDropin{
			{
				Name:     name,
				Contents: contents,
			},
		},
	})
}

func (c *Conf) addSystemdDropinV3(service, name, contents string) {
	newConfig := v3types.Config{
		Ignition: v3types.Ignition{
			Version: "3.0.0",
		},
		Systemd: v3types.Systemd{
			Units: []v3types.Unit{
				{
					Name: service,
					Dropins: []v3types.Dropin{
						{
							Name:     name,
							Contents: &contents,
						},
					},
				},
			},
		},
	}
	c.MergeV3(newConfig)
}

func (c *Conf) addSystemdDropinV31(service, name, contents string) {
	newConfig := v31types.Config{
		Ignition: v31types.Ignition{
			Version: "3.1.0",
		},
		Systemd: v31types.Systemd{
			Units: []v31types.Unit{
				{
					Name: service,
					Dropins: []v31types.Dropin{
						{
							Name:     name,
							Contents: &contents,
						},
					},
				},
			},
		},
	}
	c.MergeV31(newConfig)
}

func (c *Conf) addSystemdDropinV32(service, name, contents string) {
	newConfig := v32types.Config{
		Ignition: v32types.Ignition{
			Version: "3.2.0",
		},
		Systemd: v32types.Systemd{
			Units: []v32types.Unit{
				{
					Name: service,
					Dropins: []v32types.Dropin{
						{
							Name:     name,
							Contents: &contents,
						},
					},
				},
			},
		},
	}
	c.MergeV32(newConfig)
}

func (c *Conf) addSystemdDropinV33(service, name, contents string) {
	newConfig := v33types.Config{
		Ignition: v33types.Ignition{
			Version: "3.3.0",
		},
		Systemd: v33types.Systemd{
			Units: []v33types.Unit{
				{
					Name: service,
					Dropins: []v33types.Dropin{
						{
							Name:     name,
							Contents: &contents,
						},
					},
				},
			},
		},
	}
	c.MergeV33(newConfig)
}

func (c *Conf) addSystemdDropinCloudConfig(service, name, contents string) {
	for i, unit := range c.cloudconfig.CoreOS.Units {
		if unit.Name == service {
			unit.DropIns = append(unit.DropIns, cci.UnitDropIn{
				Name:    name,
				Content: contents,
			})
			c.cloudconfig.CoreOS.Units[i] = unit
			return
		}
	}
	c.cloudconfig.CoreOS.Units = append(c.cloudconfig.CoreOS.Units, cci.Unit{
		Name: service,
		DropIns: []cci.UnitDropIn{
			{
				Name:    name,
				Content: contents,
			},
		},
	})
}

func (c *Conf) AddSystemdUnitDropin(service, name, contents string) {
	if c.ignitionV1 != nil {
		c.addSystemdDropinV1(service, name, contents)
	} else if c.ignitionV2 != nil {
		c.addSystemdDropinV2(service, name, contents)
	} else if c.ignitionV21 != nil {
		c.addSystemdDropinV21(service, name, contents)
	} else if c.ignitionV22 != nil {
		c.addSystemdDropinV22(service, name, contents)
	} else if c.ignitionV23 != nil {
		c.addSystemdDropinV23(service, name, contents)
	} else if c.ignitionV3 != nil {
		c.addSystemdDropinV3(service, name, contents)
	} else if c.ignitionV31 != nil {
		c.addSystemdDropinV3(service, name, contents)
	} else if c.ignitionV32 != nil {
		c.addSystemdDropinV32(service, name, contents)
	} else if c.ignitionV33 != nil {
		c.addSystemdDropinV33(service, name, contents)
	} else if c.cloudconfig != nil {
		c.addSystemdDropinCloudConfig(service, name, contents)
	}
}

func (c *Conf) copyKeysIgnitionV1(keys []*agent.Key) {
	keyStrs := keysToStrings(keys)
	for i := range c.ignitionV1.Passwd.Users {
		user := &c.ignitionV1.Passwd.Users[i]
		if user.Name == c.user {
			user.SSHAuthorizedKeys = append(user.SSHAuthorizedKeys, keyStrs...)
			return
		}
	}
	c.ignitionV1.Passwd.Users = append(c.ignitionV1.Passwd.Users, v1types.User{
		Name:              c.user,
		SSHAuthorizedKeys: keyStrs,
	})
}

func (c *Conf) copyKeysIgnitionV2(keys []*agent.Key) {
	keyStrs := keysToStrings(keys)
	for i := range c.ignitionV2.Passwd.Users {
		user := &c.ignitionV2.Passwd.Users[i]
		if user.Name == c.user {
			user.SSHAuthorizedKeys = append(user.SSHAuthorizedKeys, keyStrs...)
			return
		}
	}
	c.ignitionV2.Passwd.Users = append(c.ignitionV2.Passwd.Users, v2types.User{
		Name:              c.user,
		SSHAuthorizedKeys: keyStrs,
	})
}

func (c *Conf) copyKeysIgnitionV21(keys []*agent.Key) {
	var keyObjs []v21types.SSHAuthorizedKey
	for _, key := range keys {
		keyObjs = append(keyObjs, v21types.SSHAuthorizedKey(key.String()))
	}
	for i := range c.ignitionV21.Passwd.Users {
		user := &c.ignitionV21.Passwd.Users[i]
		if user.Name == c.user {
			user.SSHAuthorizedKeys = append(user.SSHAuthorizedKeys, keyObjs...)
			return
		}
	}
	c.ignitionV21.Passwd.Users = append(c.ignitionV21.Passwd.Users, v21types.PasswdUser{
		Name:              c.user,
		SSHAuthorizedKeys: keyObjs,
	})
}

func (c *Conf) copyKeysIgnitionV22(keys []*agent.Key) {
	var keyObjs []v22types.SSHAuthorizedKey
	for _, key := range keys {
		keyObjs = append(keyObjs, v22types.SSHAuthorizedKey(key.String()))
	}
	for i := range c.ignitionV22.Passwd.Users {
		user := &c.ignitionV22.Passwd.Users[i]
		if user.Name == c.user {
			user.SSHAuthorizedKeys = append(user.SSHAuthorizedKeys, keyObjs...)
			return
		}
	}
	c.ignitionV22.Passwd.Users = append(c.ignitionV22.Passwd.Users, v22types.PasswdUser{
		Name:              c.user,
		SSHAuthorizedKeys: keyObjs,
	})
}

func (c *Conf) copyKeysIgnitionV23(keys []*agent.Key) {
	var keyObjs []v23types.SSHAuthorizedKey
	for _, key := range keys {
		keyObjs = append(keyObjs, v23types.SSHAuthorizedKey(key.String()))
	}
	for i := range c.ignitionV23.Passwd.Users {
		user := &c.ignitionV23.Passwd.Users[i]
		if user.Name == c.user {
			user.SSHAuthorizedKeys = append(user.SSHAuthorizedKeys, keyObjs...)
			return
		}
	}
	c.ignitionV23.Passwd.Users = append(c.ignitionV23.Passwd.Users, v23types.PasswdUser{
		Name:              c.user,
		SSHAuthorizedKeys: keyObjs,
	})
}

func (c *Conf) copyKeysIgnitionV3(keys []*agent.Key) {
	var keyObjs []v3types.SSHAuthorizedKey
	for _, key := range keys {
		keyObjs = append(keyObjs, v3types.SSHAuthorizedKey(key.String()))
	}
	newConfig := v3types.Config{
		Ignition: v3types.Ignition{
			Version: "3.0.0",
		},
		Passwd: v3types.Passwd{
			Users: []v3types.PasswdUser{
				{
					Name:              c.user,
					SSHAuthorizedKeys: keyObjs,
				},
			},
		},
	}
	c.MergeV3(newConfig)
}

func (c *Conf) copyKeysIgnitionV31(keys []*agent.Key) {
	var keyObjs []v31types.SSHAuthorizedKey
	for _, key := range keys {
		keyObjs = append(keyObjs, v31types.SSHAuthorizedKey(key.String()))
	}
	newConfig := v31types.Config{
		Ignition: v31types.Ignition{
			Version: "3.1.0",
		},
		Passwd: v31types.Passwd{
			Users: []v31types.PasswdUser{
				{
					Name:              c.user,
					SSHAuthorizedKeys: keyObjs,
				},
			},
		},
	}
	c.MergeV31(newConfig)
}

func (c *Conf) copyKeysIgnitionV32(keys []*agent.Key) {
	var keyObjs []v32types.SSHAuthorizedKey
	for _, key := range keys {
		keyObjs = append(keyObjs, v32types.SSHAuthorizedKey(key.String()))
	}
	newConfig := v32types.Config{
		Ignition: v32types.Ignition{
			Version: "3.2.0",
		},
		Passwd: v32types.Passwd{
			Users: []v32types.PasswdUser{
				{
					Name:              c.user,
					SSHAuthorizedKeys: keyObjs,
				},
			},
		},
	}
	c.MergeV32(newConfig)
}

func (c *Conf) copyKeysIgnitionV33(keys []*agent.Key) {
	var keyObjs []v33types.SSHAuthorizedKey
	for _, key := range keys {
		keyObjs = append(keyObjs, v33types.SSHAuthorizedKey(key.String()))
	}
	newConfig := v33types.Config{
		Ignition: v33types.Ignition{
			Version: "3.3.0",
		},
		Passwd: v33types.Passwd{
			Users: []v33types.PasswdUser{
				{
					Name:              c.user,
					SSHAuthorizedKeys: keyObjs,
				},
			},
		},
	}
	c.MergeV33(newConfig)
}

func (c *Conf) copyKeysCloudConfig(keys []*agent.Key) {
	c.cloudconfig.SSHAuthorizedKeys = append(c.cloudconfig.SSHAuthorizedKeys, keysToStrings(keys)...)
}

func (c *Conf) copyKeysScript(keys []*agent.Key) {
	keyString := strings.Join(keysToStrings(keys), "\n")
	c.script = strings.Replace(c.script, "@SSH_KEYS@", keyString, -1)
}

// CopyKeys copies public keys from agent ag into the configuration to the
// appropriate configuration section for the core user.
func (c *Conf) CopyKeys(keys []*agent.Key) {
	if c.ignitionV1 != nil {
		c.copyKeysIgnitionV1(keys)
	} else if c.ignitionV2 != nil {
		c.copyKeysIgnitionV2(keys)
	} else if c.ignitionV21 != nil {
		c.copyKeysIgnitionV21(keys)
	} else if c.ignitionV22 != nil {
		c.copyKeysIgnitionV22(keys)
	} else if c.ignitionV23 != nil {
		c.copyKeysIgnitionV23(keys)
	} else if c.ignitionV3 != nil {
		c.copyKeysIgnitionV3(keys)
	} else if c.ignitionV31 != nil {
		c.copyKeysIgnitionV31(keys)
	} else if c.ignitionV32 != nil {
		c.copyKeysIgnitionV32(keys)
	} else if c.ignitionV33 != nil {
		c.copyKeysIgnitionV33(keys)
	} else if c.cloudconfig != nil {
		c.copyKeysCloudConfig(keys)
	} else if c.script != "" {
		c.copyKeysScript(keys)
	}
}

func keysToStrings(keys []*agent.Key) (keyStrs []string) {
	for _, key := range keys {
		keyStrs = append(keyStrs, key.String())
	}
	return
}

// IsIgnition returns true if the config is for Ignition.
// Returns false in the case of empty configs as on most platforms,
// this will default back to cloudconfig
func (c *Conf) IsIgnition() bool {
	return c.ignitionV1 != nil || c.ignitionV2 != nil || c.ignitionV21 != nil || c.ignitionV22 != nil || c.ignitionV23 != nil || c.ignitionV3 != nil || c.ignitionV31 != nil || c.ignitionV32 != nil || c.ignitionV33 != nil
}

func (c *Conf) IsEmpty() bool {
	return !c.IsIgnition() && c.cloudconfig == nil && c.script == ""
}

func AddSSHKeys(userdata *UserData, keys *[]agent.Key) *UserData {
	for _, key := range *keys {
		userdata = userdata.AddKey(key)
	}
	return userdata
}

// AddUserToGroups add the user to the given groups
func (c *Conf) AddUserToGroups(user string, groups []string) error {
	var err error

	if c.ignitionV3 != nil {
		c.addUserToGroupsV3(user, groups)
	} else if c.ignitionV31 != nil {
		c.addUserToGroupsV31(user, groups)
	} else if c.ignitionV32 != nil {
		c.addUserToGroupsV32(user, groups)
	} else if c.ignitionV33 != nil {
		c.addUserToGroupsV33(user, groups)
	} else {
		err = fmt.Errorf("missing addUserToGroups implementation for this config type")
	}

	return err
}

func (c *Conf) addUserToGroupsV3(user string, groups []string) {
	g := []v3types.Group{}
	for _, group := range groups {
		g = append(g, v3types.Group(group))
	}

	newConfig := v3types.Config{
		Ignition: v3types.Ignition{
			Version: "3.0.0",
		},
		Passwd: v3types.Passwd{
			Users: []v3types.PasswdUser{
				{
					Name:   user,
					Groups: g,
				},
			},
		},
	}
	c.MergeV3(newConfig)
}

func (c *Conf) addUserToGroupsV31(user string, groups []string) {
	g := []v31types.Group{}
	for _, group := range groups {
		g = append(g, v31types.Group(group))
	}

	newConfig := v31types.Config{
		Ignition: v31types.Ignition{
			Version: "3.1.0",
		},
		Passwd: v31types.Passwd{
			Users: []v31types.PasswdUser{
				{
					Name:   user,
					Groups: g,
				},
			},
		},
	}
	c.MergeV31(newConfig)
}

func (c *Conf) addUserToGroupsV32(user string, groups []string) {
	g := []v32types.Group{}
	for _, group := range groups {
		g = append(g, v32types.Group(group))
	}

	newConfig := v32types.Config{
		Ignition: v32types.Ignition{
			Version: "3.2.0",
		},
		Passwd: v32types.Passwd{
			Users: []v32types.PasswdUser{
				{
					Name:   user,
					Groups: g,
				},
			},
		},
	}
	c.MergeV32(newConfig)
}

func (c *Conf) addUserToGroupsV33(user string, groups []string) {
	g := []v33types.Group{}
	for _, group := range groups {
		g = append(g, v33types.Group(group))
	}

	newConfig := v33types.Config{
		Ignition: v33types.Ignition{
			Version: "3.3.0",
		},
		Passwd: v33types.Passwd{
			Users: []v33types.PasswdUser{
				{
					Name:   user,
					Groups: g,
				},
			},
		},
	}
	c.MergeV33(newConfig)
}
