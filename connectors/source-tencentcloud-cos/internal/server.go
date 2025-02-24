// Copyright 2022 Linkall Inc.
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

package internal

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"time"

	cdkgo "github.com/linkall-labs/cdk-go"
	"github.com/linkall-labs/cdk-go/log"
	"github.com/pkg/errors"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	v20180416 "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/scf/v20180416"
)

const (
	name = "Tencent Cloud COS Source"
)

var (
	runtime            = "Go1"
	handler            = "main"
	funcDesc           = "auto-created function by Vanus for syncing COS event"
	funcMemSize        = int64(64)
	functionNamePrefix = "vanus-cos-source-function"
	defaultFunction    = Code{
		Bucket: "vanus-1253760853",
		Region: "ap-beijing",
		Path:   "/vanus/cos-source/dev/main.zip",
	}

	triggerType   = "cos"
	triggerEnable = "OPEN"
	triggerDesc   = `{"event":"cos:ObjectCreated:*"}`
)

var _ cdkgo.SourceConfigAccessor = &cosConfig{}

type cosConfig struct {
	cdkgo.SourceConfig `json:",inline" yaml:",inline"`
	B                  Bucket   `json:"bucket" yaml:"bucket"`
	F                  Function `json:"function" yaml:"function"`
	Debug              bool     `json:"debug" yaml:"debug"`
	Eventbus           string   `json:"eventbus" yaml:"eventbus"`
	Secret             *Secret  `json:"secret" yaml:"secret"`
}

func (c *cosConfig) GetSecret() cdkgo.SecretAccessor {
	return c.Secret
}

func NewConfig() cdkgo.SourceConfigAccessor {
	return &cosConfig{
		Secret: &Secret{},
	}
}

type Bucket struct {
	Endpoint string      `json:"endpoint" yaml:"endpoint"`
	Filters  interface{} `json:"filters" yaml:"filters"`
}

type Function struct {
	Name      string `json:"name" yaml:"name"`
	Namespace string `json:"namespace" yaml:"namespace"`
	Region    string `json:"region"  yaml:"region"`
	C         Code   `json:"code" yaml:"code"`
}

func (f Function) isValid() bool {
	return f.Region != ""
}

type Code struct {
	Bucket string `yaml:"bucket" json:"bucket"`
	Region string `yaml:"region" json:"region"`
	Path   string `yaml:"path" json:"path"`
}

func (c Code) isValid() bool {
	return c.Bucket != "" && c.Region != "" && c.Path != ""
}

type Secret struct {
	SecretID  string `json:"secret_id" yaml:"secret_id"`
	SecretKey string `json:"secret_key" yaml:"secret_key"`
}

var _ cdkgo.Source = &cosSource{}

func NewCosSink() cdkgo.Source {
	return &cosSource{}
}

type cosSource struct {
	scfClient *v20180416.Client
	logger    log.Logger
	cfg       *cosConfig
	mutex     sync.Mutex
}

func (c *cosSource) Chan() <-chan *cdkgo.Tuple {
	// It's unnecessary for COS Source
	return make(chan *cdkgo.Tuple, 0)
}

func (c *cosSource) Initialize(_ context.Context, cfg cdkgo.ConfigAccessor) error {
	_cfg, ok := cfg.(*cosConfig)
	if !ok {
		return errors.New("invalid config")
	}

	if _cfg.F.Name == "" {
		r := rand.New(rand.NewSource(time.Now().Unix()))
		_cfg.F.Name = fmt.Sprintf("%s-%d", functionNamePrefix, r.Uint64())
	}

	if _cfg.F.Namespace == "" {
		_cfg.F.Namespace = "default"
	}

	if !_cfg.F.C.isValid() {
		_cfg.F.C = defaultFunction
	}
	c.cfg = _cfg

	cli, err := v20180416.NewClient(&common.Credential{
		SecretId:  c.cfg.Secret.SecretID,
		SecretKey: c.cfg.Secret.SecretKey,
	}, c.cfg.F.Region, profile.NewClientProfile())

	if err != nil {
		return err
	}

	c.scfClient = cli

	return c.Run()
}

func (c *cosSource) Name() string {
	return name
}

func (c *cosSource) Run() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// TODO 检查cos配置
	debugStr := strconv.FormatBool(c.cfg.Debug)
	req := v20180416.NewCreateFunctionRequest()
	req.FunctionName = &c.cfg.F.Name
	req.Description = &funcDesc
	req.MemorySize = &funcMemSize
	req.Runtime = &runtime
	req.Handler = &handler
	req.Namespace = &c.cfg.F.Namespace
	req.Environment = &v20180416.Environment{
		Variables: []*v20180416.Variable{
			{
				Key:   &EnvEventGateway,
				Value: &c.cfg.Target,
			},
			{
				Key:   &EnvFuncName,
				Value: &c.cfg.F.Name,
			},
			{
				Key:   &EnvVanusEventbus,
				Value: &c.cfg.Eventbus,
			},
			{
				Key:   &EnvDebugMode,
				Value: &debugStr,
			},
		},
	}

	req.Code = &v20180416.Code{
		CosBucketName:   &c.cfg.F.C.Bucket,
		CosBucketRegion: &c.cfg.F.C.Region,
		CosObjectName:   &c.cfg.F.C.Path,
	}

	res, err := c.scfClient.CreateFunction(req)
	if err != nil {
		return err
	}

	log.Info("success to create function", map[string]interface{}{
		"response":      res.ToJsonString(),
		"function_name": c.cfg.F.Name,
	})

	for {
		getReq := v20180416.NewGetFunctionRequest()
		getReq.FunctionName = &c.cfg.F.Name
		getReq.Namespace = &c.cfg.F.Namespace
		getRes, err := c.scfClient.GetFunction(getReq)
		if err != nil {
			return err
		}
		if *getRes.Response.Status == "Active" {
			break
		}
		log.Info("function isn't ready", map[string]interface{}{
			"function_name": c.cfg.F.Name,
			"status":        *getRes.Response.Status,
		})
		time.Sleep(time.Second)
	}

	log.Info("function is ready to create trigger", map[string]interface{}{
		"function_name": c.cfg.F.Name,
	})

	createTriggerReq := v20180416.NewCreateTriggerRequest()
	createTriggerReq.FunctionName = &c.cfg.F.Name
	createTriggerReq.Namespace = &c.cfg.F.Namespace
	createTriggerReq.TriggerName = &c.cfg.B.Endpoint
	createTriggerReq.Type = &triggerType
	createTriggerReq.TriggerDesc = &triggerDesc
	createTriggerReq.Enable = &triggerEnable

	createTriggerRes, err := c.scfClient.CreateTrigger(createTriggerReq)
	if err != nil {
		return err
	}

	log.Info("success to create trigger", map[string]interface{}{
		"response":      createTriggerRes.ToJsonString(),
		"function_name": c.cfg.F.Name,
	})
	return nil
}

func (c *cosSource) Destroy() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	req := v20180416.NewDeleteFunctionRequest()
	req.FunctionName = &c.cfg.F.Name
	res, err := c.scfClient.DeleteFunction(req)
	if err != nil {
		return err
	}
	log.Info("success to delete function", map[string]interface{}{
		"response":      res.ToJsonString(),
		"function_name": c.cfg.F.Name,
	})
	return nil
}
