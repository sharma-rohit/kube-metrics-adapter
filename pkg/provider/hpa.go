package provider

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/kubernetes-incubator/custom-metrics-apiserver/pkg/provider"
	"github.com/mikkeloscar/kube-metrics-adapter/pkg/collector"
	autoscalingv2beta1 "k8s.io/api/autoscaling/v2beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/metrics/pkg/apis/custom_metrics"
	"k8s.io/metrics/pkg/apis/external_metrics"
)

type objectCollector struct {
	ObjectReference *autoscalingv2beta1.CrossVersionObjectReference
}

// HPAProvider is a base provider for initializing metric collectors based on
// HPA resources.
type HPAProvider struct {
	client             kubernetes.Interface
	interval           time.Duration
	collectorScheduler *CollectorScheduler
	collectorInterval  time.Duration
	metricSink         chan metricCollection
	hpaCache           map[resourceReference]autoscalingv2beta1.HorizontalPodAutoscaler
	metricStore        *MetricStore
	collectorFactory   *collector.CollectorFactory
}

// metricCollection is a container for sending collected metrics across a
// channel.
type metricCollection struct {
	Values []collector.CollectedMetric
	Error  error
}

// NewHPAProvider initializes a new HPAProvider.
func NewHPAProvider(client kubernetes.Interface, interval, collectorInterval time.Duration, collectorFactory *collector.CollectorFactory) *HPAProvider {
	metricsc := make(chan metricCollection)
	return &HPAProvider{
		client:            client,
		interval:          interval,
		collectorInterval: collectorInterval,
		metricSink:        metricsc,
		metricStore:       NewMetricStore(),
		collectorFactory:  collectorFactory,
	}
}

// Run runs the HPA resource discovery and metric collection.
func (p *HPAProvider) Run(ctx context.Context) {
	// initialize collector table
	p.collectorScheduler = NewCollectorScheduler(ctx, p.metricSink)

	go p.collectMetrics(ctx)

	for {
		err := p.updateHPAs()
		if err != nil {
			glog.Error(err)
		}

		select {
		case <-time.After(p.interval):
		case <-ctx.Done():
			glog.Info("Stopped HPA provider.")
			return
		}
	}
}

// updateHPAs discovers all HPA resources and sets up metric collectors for new
// HPAs.
func (p *HPAProvider) updateHPAs() error {
	glog.Info("Looking for HPAs")

	hpas, err := p.client.AutoscalingV2beta1().HorizontalPodAutoscalers(metav1.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	newHPACache := make(map[resourceReference]autoscalingv2beta1.HorizontalPodAutoscaler, len(hpas.Items))

	newHPAs := 0

	for _, hpa := range hpas.Items {
		resourceRef := resourceReference{
			Name:      hpa.Name,
			Namespace: hpa.Namespace,
		}

		if cachedHPA, ok := p.hpaCache[resourceRef]; !ok || !equalHPA(cachedHPA, hpa) {
			metricConfigs, err := collector.ParseHPAMetrics(&hpa)
			if err != nil {
				glog.Errorf("Failed to parse HPA metrics: %v", err)
				continue
			}

			cache := true
			for _, config := range metricConfigs {
				interval := config.Interval
				if interval == 0 {
					interval = p.collectorInterval
				}

				collector, err := p.collectorFactory.NewCollector(&hpa, config, interval)
				if err != nil {
					// TODO: log and send event
					glog.Errorf("Failed to create new metrics collector: %v", err)
					cache = false
					continue
				}

				glog.Infof("Adding new metrics collector: %T", collector)
				p.collectorScheduler.Add(resourceRef, config.MetricTypeName, collector)
			}
			newHPAs++

			// if we get an error setting up the collectors for the
			// HPA, don't cache it, but try again later.
			if !cache {
				continue
			}
		}

		newHPACache[resourceRef] = hpa
	}

	for ref := range p.hpaCache {
		if _, ok := newHPACache[ref]; ok {
			continue
		}

		glog.V(2).Infof("Removing previously scheduled metrics collector: %s", ref)
		p.collectorScheduler.Remove(ref)
	}

	glog.Infof("Found %d new/updated HPA(s)", newHPAs)
	p.hpaCache = newHPACache
	return nil
}

// equalHPA returns true if two HPAs are identical (apart from their status).
func equalHPA(a, b autoscalingv2beta1.HorizontalPodAutoscaler) bool {
	// reset resource version to not compare it since this will change
	// whenever the status of the object is updated. We only want to
	// compare the metadata and the spec.
	a.ObjectMeta.ResourceVersion = ""
	b.ObjectMeta.ResourceVersion = ""
	return reflect.DeepEqual(a.ObjectMeta, b.ObjectMeta) && reflect.DeepEqual(a.Spec, b.Spec)
}

// collectMetrics collects all metrics from collectors and manages a central
// metric store.
func (p *HPAProvider) collectMetrics(ctx context.Context) {
	// run garbage collection every 10 minutes
	go func(ctx context.Context) {
		for {
			select {
			case <-time.After(10 * time.Minute):
				p.metricStore.RemoveExpired()
			case <-ctx.Done():
				glog.Info("Stopped metrics store garbage collection.")
				return
			}
		}
	}(ctx)

	for {
		select {
		case collection := <-p.metricSink:
			if collection.Error != nil {
				glog.Errorf("Failed to collect metrics: %v", collection.Error)
			}

			glog.Infof("Collected %d new metric(s)", len(collection.Values))
			for _, value := range collection.Values {
				switch value.Type {
				case autoscalingv2beta1.ObjectMetricSourceType, autoscalingv2beta1.PodsMetricSourceType:
					glog.Infof("Collected new custom metric '%s' (%s) for %s %s/%s",
						value.Custom.MetricName,
						value.Custom.Value.String(),
						value.Custom.DescribedObject.Kind,
						value.Custom.DescribedObject.Namespace,
						value.Custom.DescribedObject.Name,
					)
				case autoscalingv2beta1.ExternalMetricSourceType:
					glog.Infof("Collected new external metric '%s' (%s) [%s]",
						value.External.MetricName,
						value.External.Value.String(),
						labels.Set(value.External.MetricLabels).String(),
					)
				}
				p.metricStore.Insert(value)
			}
		case <-ctx.Done():
			glog.Info("Stopped metrics collection.")
			return
		}
	}
}

// GetRootScopedMetricByName returns metrics for a root scoped resource by
// name.
func (p *HPAProvider) GetRootScopedMetricByName(groupResource schema.GroupResource, name string, metricName string) (*custom_metrics.MetricValue, error) {
	metric := p.metricStore.GetMetricsByName(metricName, groupResource, "", name)
	if metric == nil {
		return nil, provider.NewMetricNotFoundForError(groupResource, metricName, name)
	}
	return metric, nil
}

// GetRootScopedMetricBySelector returns metrics for root scoped resources by
// label selector.
func (p *HPAProvider) GetRootScopedMetricBySelector(groupResource schema.GroupResource, selector labels.Selector, metricName string) (*custom_metrics.MetricValueList, error) {
	return p.metricStore.GetMetricsBySelector(metricName, groupResource, "", selector), nil
}

// GetNamespacedMetricByName returns metrics for a namespaced resource by name.
func (p *HPAProvider) GetNamespacedMetricByName(groupResource schema.GroupResource, namespace string, name string, metricName string) (*custom_metrics.MetricValue, error) {
	metric := p.metricStore.GetMetricsByName(metricName, groupResource, namespace, name)
	if metric == nil {
		return nil, provider.NewMetricNotFoundForError(groupResource, metricName, name)
	}
	return metric, nil
}

// GetNamespacedMetricBySelector returns metrics for namespaced resources by
// label selector.
func (p *HPAProvider) GetNamespacedMetricBySelector(groupResource schema.GroupResource, namespace string, selector labels.Selector, metricName string) (*custom_metrics.MetricValueList, error) {
	return p.metricStore.GetMetricsBySelector(metricName, groupResource, namespace, selector), nil
}

// ListAllMetrics list all available metrics from the provicer.
func (p *HPAProvider) ListAllMetrics() []provider.CustomMetricInfo {
	return p.metricStore.ListAllMetrics()
}

func (p *HPAProvider) GetExternalMetric(namespace string, metricName string, metricSelector labels.Selector) (*external_metrics.ExternalMetricValueList, error) {
	return p.metricStore.GetExternalMetric(namespace, metricName, metricSelector)
}

func (p *HPAProvider) ListAllExternalMetrics() []provider.ExternalMetricInfo {
	return p.metricStore.ListAllExternalMetrics()
}

type resourceReference struct {
	Name      string
	Namespace string
}

// CollectorScheduler is a scheduler for running metric collection jobs.
// It keeps track of all running collectors and stops them if they are to be
// removed.
type CollectorScheduler struct {
	ctx        context.Context
	table      map[resourceReference]map[collector.MetricTypeName]context.CancelFunc
	metricSink chan<- metricCollection
	sync.RWMutex
}

// NewCollectorScheudler initializes a new CollectorScheduler.
func NewCollectorScheduler(ctx context.Context, metricsc chan<- metricCollection) *CollectorScheduler {
	return &CollectorScheduler{
		ctx:        ctx,
		table:      map[resourceReference]map[collector.MetricTypeName]context.CancelFunc{},
		metricSink: metricsc,
	}
}

// Add adds a new collector to the collector scheduler. Once the collector is
// added it will be started to collect metrics.
func (t *CollectorScheduler) Add(resourceRef resourceReference, typeName collector.MetricTypeName, metricCollector collector.Collector) {
	t.Lock()
	defer t.Unlock()

	collectors, ok := t.table[resourceRef]
	if !ok {
		collectors = map[collector.MetricTypeName]context.CancelFunc{}
		t.table[resourceRef] = collectors
	}

	if cancelCollector, ok := collectors[typeName]; ok {
		// stop old collector
		cancelCollector()
	}

	ctx, cancel := context.WithCancel(t.ctx)
	collectors[typeName] = cancel

	// start runner for new collector
	go collectorRunner(ctx, metricCollector, t.metricSink)
}

// collectorRunner runs a collector at the desirec interval. If the passed
// context is canceled the collection will be stopped.
func collectorRunner(ctx context.Context, collector collector.Collector, metricsc chan<- metricCollection) {
	for {
		values, err := collector.GetMetrics()

		metricsc <- metricCollection{
			Values: values,
			Error:  err,
		}

		select {
		case <-time.After(collector.Interval()):
		case <-ctx.Done():
			glog.V(2).Infof("stopping collector runner...")
			return
		}
	}
}

// Remove removes a collector from the Collector schduler. The collector is
// stopped before it's removed.
func (t *CollectorScheduler) Remove(resourceRef resourceReference) {
	t.Lock()
	defer t.Unlock()

	if collectors, ok := t.table[resourceRef]; ok {
		for _, cancelCollector := range collectors {
			cancelCollector()
		}
		delete(t.table, resourceRef)
	}
}
