// SPDX-FileCopyrightText: 2021 "SAP SE or an SAP affiliate company and Gardener contributors"
//
// SPDX-License-Identifier: Apache-2.0

package instances

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"

	guuid "github.com/google/uuid"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kutils "github.com/gardener/landscaper/controller-utils/pkg/kubernetes"
	"github.com/gardener/landscaper/controller-utils/pkg/logging"
	lc "github.com/gardener/landscaper/controller-utils/pkg/logging/constants"

	coreconfig "github.com/gardener/landscaper-service/pkg/apis/config"
	lssv1alpha1 "github.com/gardener/landscaper-service/pkg/apis/core/v1alpha1"
	lsserrors "github.com/gardener/landscaper-service/pkg/apis/errors"
	"github.com/gardener/landscaper-service/pkg/controllers/healthwatcher"
	"github.com/gardener/landscaper-service/pkg/operation"
	"github.com/gardener/landscaper-service/pkg/utils"
)

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz")

// Controller is the instances controller
type Controller struct {
	operation.Operation
	log             logging.Logger
	reconcileHelper *automaticReconcileHelper

	UniqueIDFunc func() string

	ReconcileFunc    func(ctx context.Context, instance *lssv1alpha1.Instance) error
	HandleDeleteFunc func(ctx context.Context, instance *lssv1alpha1.Instance) (reconcile.Result, error)
	ListShootsFunc   func(ctx context.Context, instance *lssv1alpha1.Instance) (*unstructured.UnstructuredList, error)

	kubeClientExtractor healthwatcher.ServiceTargetConfigKubeClientExtractorInterface
}

// NewUniqueID creates a new unique id string with a length of 8.
func (c *Controller) NewUniqueID() string {
	id := c.UniqueIDFunc()
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

func defaultUniqueIdFunc() string {
	// it must be prevented that the first 8 chars are numbers
	// this part is used for generating the shoot name but also machine names of a shoot and this is not allowed

	id := guuid.New().String()
	id = id[1:]
	s := string(letterRunes[rand.Intn(len(letterRunes))])
	t := s + id
	return t
}

// NewController returns a new instances controller
func NewController(logger logging.Logger, c client.Client, scheme *runtime.Scheme, config *coreconfig.LandscaperServiceConfiguration) (reconcile.Reconciler, error) {
	ctrl := &Controller{
		log:                 logger,
		UniqueIDFunc:        defaultUniqueIdFunc,
		reconcileHelper:     newAutomaticReconcileHelper(c, clock.RealClock{}),
		kubeClientExtractor: &healthwatcher.ServiceTargetConfigKubeClientExtractor{},
	}
	ctrl.ReconcileFunc = ctrl.reconcile
	ctrl.HandleDeleteFunc = ctrl.handleDelete
	ctrl.ListShootsFunc = ctrl.listShoots
	op := operation.NewOperation(c, scheme, config)
	ctrl.Operation = *op
	return ctrl, nil
}

type TestKubeClientExtractor struct {
}

func (t *TestKubeClientExtractor) GetKubeClientFromServiceTargetConfig(_ context.Context, _ string, _ string, client client.Client) (client.Client, error) {
	return client, nil
}

// NewTestActuator creates a new controller for testing purposes.
func NewTestActuator(op operation.Operation, logger logging.Logger) *Controller {
	ctrl := &Controller{
		Operation:           op,
		log:                 logger,
		UniqueIDFunc:        defaultUniqueIdFunc,
		reconcileHelper:     newAutomaticReconcileHelper(op.Client(), clock.RealClock{}),
		kubeClientExtractor: &TestKubeClientExtractor{},
	}
	ctrl.ReconcileFunc = ctrl.reconcile
	ctrl.HandleDeleteFunc = ctrl.handleDelete
	ctrl.ListShootsFunc = ctrl.listShoots
	return ctrl
}

// Reconcile reconciles requests for instances
func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger, ctx := c.log.StartReconcileAndAddToContext(ctx, req)

	instance := &lssv1alpha1.Instance{}
	if err := c.Client().Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info(err.Error())
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	c.Operation.Scheme().Default(instance)
	errHdl := c.handleErrorFunc(instance)

	// update observed generation
	if instance.Status.ObservedGeneration < instance.GetGeneration() {
		instance.Status.ObservedGeneration = instance.GetGeneration()
		if err := c.Client().Status().Update(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
	}

	// set finalizer
	if instance.DeletionTimestamp.IsZero() && !kutils.HasFinalizer(instance, lssv1alpha1.LandscaperServiceFinalizer) {
		controllerutil.AddFinalizer(instance, lssv1alpha1.LandscaperServiceFinalizer)
		if err := c.Client().Update(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	// reconcile delete
	if !instance.DeletionTimestamp.IsZero() {
		result, err := c.HandleDeleteFunc(ctx, instance)
		return result, errHdl(ctx, err)
	}

	if utils.HasOperationAnnotation(instance, lssv1alpha1.LandscaperServiceOperationIgnore) {
		logger.Info("instance has ignore annotation, skipping reconcile")
		return c.reconcileHelper.computeAutomaticReconcile(ctx, instance, nil)
	}

	// reconcile
	return c.reconcileHelper.computeAutomaticReconcile(ctx, instance, errHdl(ctx, c.ReconcileFunc(ctx, instance)))
}

// handleErrorFunc updates the error status of an instance
func (c *Controller) handleErrorFunc(instance *lssv1alpha1.Instance) func(ctx context.Context, err error) error {
	old := instance.DeepCopy()
	return func(ctx context.Context, err error) error {
		logger, ctx := logging.FromContextOrNew(ctx, []interface{}{lc.KeyReconciledResource, client.ObjectKeyFromObject(instance).String()})
		instance.Status.LastError = lsserrors.TryUpdateError(instance.Status.LastError, err)

		if !reflect.DeepEqual(old.Status, instance.Status) {
			if err2 := c.Client().Status().Update(ctx, instance); err2 != nil {
				if apierrors.IsConflict(err2) {
					// reduce logging
					logger.Info(fmt.Sprintf("unable to update status: %s", err2.Error()))
				} else {
					logger.Error(err2, "unable to update status")
				}

				// retry on conflict
				if err != nil {
					return err2
				}
			}
		}
		return err
	}
}
