package exoscaleimport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	egoscale "github.com/exoscale/egoscale/v2"
	exoapi "github.com/exoscale/egoscale/v2/api"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/version"
)

const (
	qemuBuilderID     = "transcend.qemu"
	fileBuilderID     = "packer.file"
	artificeBuilderID = "packer.post-processor.artifice"
)

func init() {
	egoscale.UserAgent = fmt.Sprintf("Exoscale-Packer-Post-Processor/%s %s",
		version.SDKVersion.FormattedVersion(), egoscale.UserAgent)
}

type PostProcessor struct {
	config *Config
	runner multistep.Runner
	exo    *egoscale.Client
	sos    *s3.Client
}

func (p *PostProcessor) Configure(raws ...interface{}) error {
	config, err := NewConfig(raws...)
	if err != nil {
		return err
	}
	p.config = config

	packer.LogSecretFilter.Set(p.config.APIKey, p.config.APISecret)

	return nil
}

func (p *PostProcessor) PostProcess(ctx context.Context, ui packer.Ui, a packer.Artifact) (packer.Artifact, bool, bool, error) {
	switch a.BuilderId() {
	case qemuBuilderID, fileBuilderID, artificeBuilderID:
		break
	default:
		err := fmt.Errorf("unsupported artifact type %q: this post-processor only imports "+
			"artifacts from QEMU/file builders and Artifice post-processor", a.BuilderId())
		return nil, false, false, err
	}

	exo, err := egoscale.NewClient(
		p.config.APIKey,
		p.config.APISecret,

		// Template registration can take a _long time_, raising
		// the Exoscale API client timeout as a precaution.
		egoscale.ClientOptWithTimeout(30*time.Minute),
	)
	if err != nil {
		return nil, false, false, fmt.Errorf("unable to initialize Exoscale client: %v", err)
	}
	p.exo = exo

	cfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(p.config.TemplateZone),

		awsconfig.WithEndpointResolver(aws.EndpointResolverFunc(
			func(service, region string) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:           p.config.SOSEndpoint,
					SigningRegion: p.config.TemplateZone,
				}, nil
			})),

		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			p.config.APIKey,
			p.config.APISecret,
			"")),
	)
	if err != nil {
		return nil, false, false, fmt.Errorf("unable to initialize SOS client: %v", err)
	}

	p.sos = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	state := new(multistep.BasicStateBag)
	state.Put("config", p.config)
	state.Put("exo", p.exo)
	state.Put("sos", p.sos)
	state.Put("ui", ui)
	state.Put("artifact", a)

	steps := []multistep.Step{
		new(stepUploadImage),
		new(stepRegisterTemplate),
		new(stepDeleteImage),
	}

	ctx = exoapi.WithEndpoint(ctx, exoapi.NewReqEndpoint(p.config.APIEnvironment, p.config.TemplateZone))

	p.runner = commonsteps.NewRunnerWithPauseFn(steps, p.config.PackerConfig, ui, state)
	p.runner.Run(ctx, state)

	if rawErr, ok := state.GetOk("error"); ok {
		return nil, false, false, rawErr.(error)
	}

	if _, ok := state.GetOk(multistep.StateCancelled); ok {
		return nil, false, false, errors.New("post-processing cancelled")
	}

	if _, ok := state.GetOk(multistep.StateHalted); ok {
		return nil, false, false, errors.New("post-processing halted")
	}

	v, ok := state.GetOk("template")
	if !ok {
		return nil, false, false, errors.New("unable to find template in state")
	}

	return &Artifact{
		state:    state,
		template: v.(*egoscale.Template),
	}, false, false, nil
}
