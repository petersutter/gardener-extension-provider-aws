// Copyright (c) 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package dnsrecord

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/runtime/inject"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/extensions/pkg/controller/common"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	gardencorev1beta1helper "github.com/gardener/gardener/pkg/apis/core/v1beta1/helper"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"github.com/gardener/gardener/pkg/controllerutils"
	reconcilerutils "github.com/gardener/gardener/pkg/controllerutils/reconciler"
	"github.com/gardener/gardener/pkg/extensions"
	"github.com/gardener/gardener/pkg/operation/botanist"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
)

type reconciler struct {
	logger   logr.Logger
	actuator Actuator

	client        client.Client
	reader        client.Reader
	statusUpdater extensionscontroller.StatusUpdaterCustom
}

// NewReconciler creates a new reconcile.Reconciler that reconciles
// dnsrecord resources of Gardener's `extensions.gardener.cloud` API group.
func NewReconciler(actuator Actuator) reconcile.Reconciler {
	logger := log.Log.WithName(ControllerName)

	return reconcilerutils.OperationAnnotationWrapper(
		func() client.Object { return &extensionsv1alpha1.DNSRecord{} },
		&reconciler{
			logger:        logger,
			actuator:      actuator,
			statusUpdater: extensionscontroller.NewStatusUpdater(logger),
		},
	)
}

func (r *reconciler) InjectFunc(f inject.Func) error {
	return f(r.actuator)
}

func (r *reconciler) InjectClient(client client.Client) error {
	r.client = client
	r.statusUpdater.InjectClient(client)
	return nil
}

func (r *reconciler) InjectAPIReader(reader client.Reader) error {
	r.reader = reader
	return nil
}

func (r *reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	dns := &extensionsv1alpha1.DNSRecord{}
	if err := r.client.Get(ctx, request.NamespacedName, dns); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	logger := r.logger.WithValues("dnsrecord", kutil.ObjectName(dns))
	var cluster *extensions.Cluster
	if dns.Namespace != v1beta1constants.GardenNamespace {
		var err error
		cluster, err = extensionscontroller.GetCluster(ctx, r.client, dns.Namespace)
		if err != nil {
			return reconcile.Result{}, err
		}

		if extensionscontroller.IsFailed(cluster) {
			logger.Info("Skipping the reconciliation of dnsrecord of failed shoot")
			return reconcile.Result{}, nil
		}
	}

	operationType := gardencorev1beta1helper.ComputeOperationType(dns.ObjectMeta, dns.Status.LastOperation)

	if cluster != nil && cluster.Shoot != nil && dns.Name != cluster.Shoot.Name+"-"+botanist.DNSOwnerName && operationType != gardencorev1beta1.LastOperationTypeMigrate {
		key := "dnsrecord:" + kutil.ObjectName(dns)
		ok, watchdogCtx, cleanup, err := common.GetOwnerCheckResultAndContext(ctx, r.client, dns.Namespace, cluster.Shoot.Name, key)
		if err != nil {
			return reconcile.Result{}, err
		} else if !ok {
			return reconcile.Result{}, fmt.Errorf("this seed is not the owner of shoot %s", kutil.ObjectName(cluster.Shoot))
		}
		ctx = watchdogCtx
		if cleanup != nil {
			defer cleanup()
		}
	}

	switch {
	case extensionscontroller.ShouldSkipOperation(operationType, dns):
		return reconcile.Result{}, nil
	case operationType == gardencorev1beta1.LastOperationTypeMigrate:
		return r.migrate(ctx, dns, cluster)
	case dns.DeletionTimestamp != nil:
		return r.delete(ctx, dns, cluster)
	case operationType == gardencorev1beta1.LastOperationTypeRestore:
		return r.restore(ctx, dns, cluster)
	default:
		return r.reconcile(ctx, dns, cluster, operationType)
	}
}

func (r *reconciler) reconcile(ctx context.Context, dns *extensionsv1alpha1.DNSRecord, cluster *extensionscontroller.Cluster, operationType gardencorev1beta1.LastOperationType) (reconcile.Result, error) {
	if err := controllerutils.EnsureFinalizer(ctx, r.reader, r.client, dns, FinalizerName); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.statusUpdater.ProcessingCustom(ctx, dns, operationType, "Reconciling the dnsrecord", nil); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Starting the reconciliation of dnsrecord", "dnsrecord", kutil.ObjectName(dns))
	if err := r.actuator.Reconcile(ctx, dns, cluster); err != nil {
		_ = r.statusUpdater.ErrorCustom(ctx, dns, reconcilerutils.ReconcileErrCauseOrErr(err), operationType, "Error reconciling dnsrecord", addCreatedConditionFalse)
		return reconcilerutils.ReconcileErr(err)
	}

	if err := r.statusUpdater.SuccessCustom(ctx, dns, operationType, "Successfully reconciled dnsrecord", addCreatedConditionTrue); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) restore(ctx context.Context, dns *extensionsv1alpha1.DNSRecord, cluster *extensionscontroller.Cluster) (reconcile.Result, error) {
	if err := controllerutils.EnsureFinalizer(ctx, r.reader, r.client, dns, FinalizerName); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.statusUpdater.ProcessingCustom(ctx, dns, gardencorev1beta1.LastOperationTypeRestore, "Restoring the dnsrecord", nil); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Starting the restoration of dnsrecord", "dnsrecord", kutil.ObjectName(dns))
	if err := r.actuator.Restore(ctx, dns, cluster); err != nil {
		_ = r.statusUpdater.ErrorCustom(ctx, dns, reconcilerutils.ReconcileErrCauseOrErr(err), gardencorev1beta1.LastOperationTypeRestore, "Error restoring dnsrecord", addCreatedConditionFalse)
		return reconcilerutils.ReconcileErr(err)
	}

	if err := r.statusUpdater.SuccessCustom(ctx, dns, gardencorev1beta1.LastOperationTypeRestore, "Successfully restored dnsrecord", addCreatedConditionTrue); err != nil {
		return reconcile.Result{}, err
	}

	if err := extensionscontroller.RemoveAnnotation(ctx, r.client, dns, v1beta1constants.GardenerOperation); err != nil {
		return reconcile.Result{}, fmt.Errorf("error removing annotation from dnsrecord: %+v", err)
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) migrate(ctx context.Context, dns *extensionsv1alpha1.DNSRecord, cluster *extensionscontroller.Cluster) (reconcile.Result, error) {
	if err := r.statusUpdater.ProcessingCustom(ctx, dns, gardencorev1beta1.LastOperationTypeMigrate, "Migrating the dnsrecord", nil); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Starting the migration of dnsrecord", "dnsrecord", kutil.ObjectName(dns))
	if err := r.actuator.Migrate(ctx, dns, cluster); err != nil {
		_ = r.statusUpdater.ErrorCustom(ctx, dns, reconcilerutils.ReconcileErrCauseOrErr(err), gardencorev1beta1.LastOperationTypeMigrate, "Error migrating dnsrecord", nil)
		return reconcilerutils.ReconcileErr(err)
	}

	if err := r.statusUpdater.SuccessCustom(ctx, dns, gardencorev1beta1.LastOperationTypeMigrate, "Successfully migrated dnsrecord", nil); err != nil {
		return reconcile.Result{}, err
	}

	r.logger.Info("Removing all finalizers", "dnsrecord", kutil.ObjectName(dns))
	if err := controllerutils.RemoveAllFinalizers(ctx, r.client, r.client, dns); err != nil {
		return reconcile.Result{}, fmt.Errorf("error removing finalizers from dnsrecord: %+v", err)
	}

	if err := extensionscontroller.RemoveAnnotation(ctx, r.client, dns, v1beta1constants.GardenerOperation); err != nil {
		return reconcile.Result{}, fmt.Errorf("error removing annotation from dnsrecord: %+v", err)
	}

	return reconcile.Result{}, nil
}

func (r *reconciler) delete(ctx context.Context, dns *extensionsv1alpha1.DNSRecord, cluster *extensionscontroller.Cluster) (reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(dns, FinalizerName) {
		r.logger.Info("Deleting dnsrecord causes a no-op as there is no finalizer", "dnsrecord", kutil.ObjectName(dns))
		return reconcile.Result{}, nil
	}

	switch getCreatedConditionStatus(dns.GetExtensionStatus()) {
	case gardencorev1beta1.ConditionTrue, gardencorev1beta1.ConditionUnknown:
		operationType := gardencorev1beta1helper.ComputeOperationType(dns.ObjectMeta, dns.Status.LastOperation)
		if err := r.statusUpdater.ProcessingCustom(ctx, dns, operationType, "Deleting the dnsrecord", nil); err != nil {
			return reconcile.Result{}, err
		}

		r.logger.Info("Starting the deletion of dnsrecord", "dnsrecord", kutil.ObjectName(dns))
		if err := r.actuator.Delete(ctx, dns, cluster); err != nil {
			_ = r.statusUpdater.ErrorCustom(ctx, dns, reconcilerutils.ReconcileErrCauseOrErr(err), operationType, "Error deleting dnsrecord", nil)
			return reconcilerutils.ReconcileErr(err)
		}

		if err := r.statusUpdater.SuccessCustom(ctx, dns, operationType, "Successfully deleted dnsrecord", nil); err != nil {
			return reconcile.Result{}, err
		}
	case gardencorev1beta1.ConditionFalse:
		r.logger.Info("Deleting dnsrecord is no-op as not created", "dnsrecord", kutil.ObjectName(dns))
	}

	r.logger.Info("Removing finalizer", "dnsrecord", kutil.ObjectName(dns))
	if err := controllerutils.RemoveFinalizer(ctx, r.reader, r.client, dns, FinalizerName); err != nil {
		return reconcile.Result{}, fmt.Errorf("error removing finalizer from dnsrecord: %+v", err)
	}

	return reconcile.Result{}, nil
}

func updateCreatedCondition(status extensionsv1alpha1.Status, conditionStatus gardencorev1beta1.ConditionStatus, reason, message string, updateIfExisting bool) error {
	conditions := status.GetConditions()
	c := gardencorev1beta1helper.GetCondition(conditions, extensionsv1alpha1.ConditionTypeCreated)
	if c != nil && !updateIfExisting {
		return nil
	}
	if c != nil && c.Status == conditionStatus {
		return nil
	}

	builder, err := gardencorev1beta1helper.NewConditionBuilder(extensionsv1alpha1.ConditionTypeCreated)
	if err != nil {
		return err
	}
	if c != nil {
		builder = builder.WithOldCondition(*c)
	}

	new, _ := builder.WithStatus(conditionStatus).WithReason(reason).WithMessage(message).Build()
	status.SetConditions(gardencorev1beta1helper.MergeConditions(conditions, new))
	return nil
}

func getCreatedConditionStatus(status extensionsv1alpha1.Status) gardencorev1beta1.ConditionStatus {
	for _, c := range status.GetConditions() {
		if c.Type == extensionsv1alpha1.ConditionTypeCreated {
			return c.Status
		}
	}
	return gardencorev1beta1.ConditionUnknown
}

func addCreatedConditionFalse(status extensionsv1alpha1.Status) error {
	message := "Error on initial record creation in infrastructure"
	return updateCreatedCondition(status, gardencorev1beta1.ConditionFalse, "Error", message, false)
}

func addCreatedConditionTrue(status extensionsv1alpha1.Status) error {
	message := "Record was created successfully in infrastructure at least once"
	return updateCreatedCondition(status, gardencorev1beta1.ConditionTrue, "Success", message, true)
}
