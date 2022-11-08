/*
Copyright 2022 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kops

import (
	"context"
	"fmt"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/crossplane/provider-kops/apis/kops/v1alpha1"
	apisv1alpha1 "github.com/crossplane/provider-kops/apis/v1alpha1"
	"github.com/crossplane/provider-kops/internal/controller/features"
	"github.com/crossplane/provider-kops/internal/util"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kopsClient "k8s.io/kops/pkg/client/simple"
	resourceops "k8s.io/kops/pkg/resources/ops"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	errNotKops               = "managed resource is not a Kops custom resource"
	errTrackPCUsage          = "cannot track ProviderConfig usage"
	errNewClient             = "cannot create new Service"
	errDeleteCluster         = "cannot delete Kops cluster from API"
	errNewCluster            = "cannot create Kops cluster"
	errNewClusterState       = "cannot create Kops cluster state"
	errNewInstanceGroupState = "cannot create Kops instance group state"
	errNewCloud              = "cannot create Kops cloud"
	errNewCloudAssignment    = "cannot assign Kops cloud"
	errGetCluster            = "cannot get Kops cluster from API"
	errGetInstanceGroup      = "cannot get Kops instance group from API"
	errValidateCluster       = "cannot validate Kops cluster"
	errEvaluateClusterState  = "cannot evaluate Kops cluster state"
	errGetKubeConfig         = "cannot get KubeConfig"
	errGetClusterStatus      = "cannot get Kops cluster status"
	errUpdateCluster         = "cannot update Kops cluster"
	errUpdateClusterState    = "cannot update Kops cluster state"
	errDeleteResources       = "cannot delete Kops resources"
)

// Setup adds a controller that reconciles Kops managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.KopsGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.KopsGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:  mgr.GetClient(),
			usage: resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{})}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.Kops{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

type connector struct {
	kube  client.Client
	usage resource.Tracker
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Kops)
	if !ok {
		return nil, errors.New(errNotKops)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	kopsClientset, err := util.GetKopsClientset(cr.Spec.ForProvider.StateBucket, meta.GetExternalName(cr), cr.Spec.ForProvider.Domain)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{kopsClientset: kopsClientset}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	service       interface{}
	kopsClientset kopsClient.Clientset
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Kops)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotKops)
	}

	cluster, err := c.kopsClientset.GetCluster(ctx, fmt.Sprintf("%v.%v", meta.GetExternalName(cr), cr.Spec.ForProvider.Domain))
	if err != nil {
		if util.ErrNotFound(err) {
			return managed.ExternalObservation{ResourceExists: false}, nil
		}
		return managed.ExternalObservation{ResourceExists: false}, errors.Wrap(err, errGetCluster)
	}

	ig, err := c.kopsClientset.InstanceGroupsFor(cluster).List(ctx, metav1.ListOptions{})
	if err != nil {
		return managed.ExternalObservation{ResourceExists: false}, errors.Wrap(err, errGetInstanceGroup)
	}

	validate, err := util.ValidateKopsCluster(c.kopsClientset, cluster, ig, cr.Spec.ForProvider.KubernetesAPICertificateTTL)
	if err != nil {
		return managed.ExternalObservation{ResourceExists: false}, errors.Wrap(err, errValidateCluster)
	}

	ok, res := util.EvaluateKopsValidationResult(validate)
	if !ok {
		return managed.ExternalObservation{ResourceExists: false}, errors.Wrap(fmt.Errorf("%s", res), errEvaluateClusterState)
	}

	kubeconfig, err := util.GenerateKubeConfig(cluster, c.kopsClientset, cr.Spec.ForProvider.KubernetesAPICertificateTTL)
	if err != nil {
		return managed.ExternalObservation{ResourceExists: false}, errors.Wrap(err, errGetKubeConfig)
	}

	conn := managed.ConnectionDetails{
		xpv1.ResourceCredentialsSecretKubeconfigKey: kubeconfig,
	}

	cr.Status.SetConditions(xpv1.Available())
	return managed.ExternalObservation{
		ResourceExists: true,
		ResourceUpToDate: (util.ClusterResourceUpToDate(&cr.Spec.ForProvider.ClusterSpec, &cluster.Spec) &&
			util.InstanceGroupListResourceUpToDate(cr.Spec.ForProvider.InstanceGroupSpec, ig)),
		ConnectionDetails: conn,
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Kops)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotKops)
	}

	cluster, err := c.kopsClientset.CreateCluster(ctx, util.CreateClusterSpec(cr))
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errNewClusterState)
	}

	for _, ig := range cr.Spec.ForProvider.InstanceGroupSpec {
		_, err := c.kopsClientset.InstanceGroupsFor(cluster).Create(ctx, util.CreateInstanceGroupSpec(ig), metav1.CreateOptions{})
		if err != nil {
			return managed.ExternalCreation{}, errors.Wrap(err, errNewInstanceGroupState)
		}
	}

	cloud, err := cloudup.BuildCloud(cluster)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errNewCloud)
	}

	if err := cloudup.PerformAssignments(cluster, cloud); err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errNewCloudAssignment)
	}

	applyCmd := &cloudup.ApplyClusterCmd{
		Cloud:      cloud,
		Cluster:    cluster,
		Clientset:  c.kopsClientset,
		TargetName: cloudup.TargetDirect,
	}

	err = applyCmd.Run(ctx)

	if err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errNewCluster)
	}

	cr.Status.SetConditions(xpv1.Creating())

	return managed.ExternalCreation{
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.Kops)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotKops)
	}

	cluster := util.CreateClusterSpec(cr)

	cloud, err := cloudup.BuildCloud(cluster)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errNewCloud)
	}

	if err := cloudup.PerformAssignments(cluster, cloud); err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errNewCloudAssignment)
	}

	status, err := util.GetClusterStatus(cluster, cloud)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errGetClusterStatus)
	}

	clusterToUpdate, err := c.kopsClientset.UpdateCluster(ctx, cluster, status)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errUpdateClusterState)
	}

	for _, ig := range cr.Spec.ForProvider.InstanceGroupSpec {
		_, err := c.kopsClientset.InstanceGroupsFor(clusterToUpdate).Update(ctx, util.CreateInstanceGroupSpec(ig), metav1.UpdateOptions{})
		if err != nil {
			return managed.ExternalUpdate{}, errors.Wrap(err, errNewInstanceGroupState)
		}
	}

	applyCmd := &cloudup.ApplyClusterCmd{
		Cloud:      cloud,
		Cluster:    clusterToUpdate,
		Clientset:  c.kopsClientset,
		TargetName: cloudup.TargetDirect,
	}

	err = applyCmd.Run(ctx)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(err, errUpdateCluster)
	}

	return managed.ExternalUpdate{
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.Kops)
	if !ok {
		return errors.New(errNotKops)
	}
	cluster, err := c.kopsClientset.GetCluster(ctx, fmt.Sprintf("%v.%v", meta.GetExternalName(cr), cr.Spec.ForProvider.Domain))
	if err != nil {
		return errors.Wrap(err, errGetCluster)
	}

	cloud, err := cloudup.BuildCloud(cluster)
	if err != nil {
		return errors.Wrap(err, errDeleteCluster)
	}

	allResources, err := resourceops.ListResources(cloud, cluster, cr.Spec.ForProvider.Region)
	if err != nil {
		return err
	}

	err = resourceops.DeleteResources(cloud, allResources)
	if err != nil {
		return errors.Wrap(err, errDeleteResources)
	}

	err = c.kopsClientset.DeleteCluster(ctx, cluster)
	if err != nil {
		return errors.Wrap(err, errDeleteCluster)
	}
	cr.Status.SetConditions(xpv1.Deleting())

	return nil
}
