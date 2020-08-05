package exec

import (
	"context"
	"fmt"
	"time"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerctx"
	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/creds"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/resource"
	"github.com/concourse/concourse/atc/runtime"
	"github.com/concourse/concourse/atc/worker"
	"github.com/concourse/concourse/tracing"
)

type CheckStep struct {
	planID            atc.PlanID
	plan              atc.CheckPlan
	metadata          StepMetadata
	containerMetadata db.ContainerMetadata
	resourceFactory   resource.ResourceFactory
	strategy          worker.ContainerPlacementStrategy
	pool              worker.Pool
	delegate          BuildStepDelegate
	succeeded         bool
	workerClient      worker.Client

	resourceConfigFactory db.ResourceConfigFactory

	// XXX: may be nil; remove when resource config scopes are removed and
	// global resources is on by default
	resource db.Resource
}

func NewCheckStep(
	planID atc.PlanID,
	plan atc.CheckPlan,
	resource db.Resource,
	resourceConfigFactory db.ResourceConfigFactory,
	metadata StepMetadata,
	resourceFactory resource.ResourceFactory,
	containerMetadata db.ContainerMetadata,
	strategy worker.ContainerPlacementStrategy,
	pool worker.Pool,
	delegate BuildStepDelegate,
	client worker.Client,
) *CheckStep {
	return &CheckStep{
		planID:                planID,
		plan:                  plan,
		resource:              resource,
		resourceConfigFactory: resourceConfigFactory,
		metadata:              metadata,
		resourceFactory:       resourceFactory,
		containerMetadata:     containerMetadata,
		pool:                  pool,
		strategy:              strategy,
		delegate:              delegate,
		workerClient:          client,
	}
}

func (step *CheckStep) Run(ctx context.Context, state RunState) error {
	ctx, span := tracing.StartSpan(ctx, "check", tracing.Attrs{
		"team":     step.metadata.TeamName,
		"pipeline": step.metadata.PipelineName,
		"job":      step.metadata.JobName,
		"build":    step.metadata.BuildName,
		"name":     step.plan.Name,
	})

	err := step.run(ctx, state)
	tracing.End(span, err)

	return err
}

type ResourceTypeImage struct {
	Cache db.UsedResourceCache
	Spec  worker.ImageSpec
}

func (step *CheckStep) run(ctx context.Context, state RunState) error {
	logger := lagerctx.FromContext(ctx)
	logger = logger.Session("check-step", lager.Data{
		"step-name": step.plan.Name,
	})

	variables := step.delegate.Variables()

	source, err := creds.NewSource(variables, step.plan.Source).Evaluate()
	if err != nil {
		return fmt.Errorf("resource config creds evaluation: %w", err)
	}

	workerSpec := worker.WorkerSpec{
		Tags:   step.plan.Tags,
		TeamID: step.metadata.TeamID,
	}

	var resourceConfig db.ResourceConfig
	var imageSpec worker.ImageSpec
	if step.plan.Type != "" {
		imageSpec.BaseResourceType = step.plan.Type
		workerSpec.BaseResourceType = step.plan.Type

		resourceConfig, err = step.resourceConfigFactory.FindOrCreateResourceConfigFromBaseType(
			step.plan.Type,
			step.plan.Source,
		)
		if err != nil {
			return fmt.Errorf("create config from base type: %w", err)
		}
	} else if step.plan.TypeFrom != nil {
		var typeImage ResourceTypeImage
		if !state.Result(*step.plan.TypeFrom, &typeImage) {
			return fmt.Errorf("other step did not provide type image")
		}

		imageSpec = typeImage.Spec

		resourceConfig, err = step.resourceConfigFactory.FindOrCreateResourceConfigFromResourceCache(
			typeImage.Cache,
			step.plan.Source,
		)
		if err != nil {
			return fmt.Errorf("create config from type image cache: %w", err)
		}
	}

	timeout, err := time.ParseDuration(step.plan.Timeout)
	if err != nil {
		return fmt.Errorf("timeout parse: %w", err)
	}

	containerSpec := worker.ContainerSpec{
		ImageSpec: imageSpec,
		BindMounts: []worker.BindMountSource{
			&worker.CertsVolumeMount{Logger: logger},
		},
		Tags:   step.plan.Tags,
		TeamID: step.metadata.TeamID,
		Env:    step.metadata.Env(),
	}
	tracing.Inject(ctx, &containerSpec)

	expires := db.ContainerOwnerExpiries{
		Min: 5 * time.Minute,
		Max: 1 * time.Hour,
	}

	owner := db.NewResourceConfigCheckSessionContainerOwner(
		resourceConfig.ID(),
		resourceConfig.OriginBaseResourceType().ID,
		expires,
	)

	checkable := step.resourceFactory.NewResource(
		source,
		nil,
		step.plan.FromVersion,
	)

	imageFetcherSpec := worker.ImageFetcherSpec{
		// ResourceTypes: resourceTypes,
		Delegate: step.delegate,
	}

	result, err := step.workerClient.RunCheckStep(
		ctx,
		logger,
		owner,
		containerSpec,
		workerSpec,
		step.strategy,

		step.containerMetadata,
		imageFetcherSpec,

		timeout,
		checkable,
	)
	if err != nil {
		return fmt.Errorf("run check step: %w", err)
	}

	scope, err := resourceConfig.FindOrCreateScope(step.resource)
	if err != nil {
		return fmt.Errorf("find or create scope: %w", err)
	}

	err = scope.SaveVersions(db.NewSpanContext(ctx), result.Versions)
	if err != nil {
		return fmt.Errorf("save versions: %w", err)
	}

	if len(result.Versions) > 0 {
		latestVersion := result.Versions[len(result.Versions)-1]
		state.StoreResult(step.planID, runtime.VersionResult{
			Version: latestVersion,
			// XXX: no metadata, not that it matters - this may change withh Prototypes
		})
	}

	step.succeeded = true

	return nil
}

func (step *CheckStep) Succeeded() bool {
	return step.succeeded
}
