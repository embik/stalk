package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/discovery/cached/disk"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

/*
	stalk -n kubermatic deployments,pods [-l "field=value"] [NAME, ...]
*/

type options struct {
	kubeconfig        string
	namespace         string
	labels            string
	hideManagedFields bool
	selector          labels.Selector
	verbose           bool
}

type resourceCache struct {
	resources map[string]*unstructured.Unstructured
}

func newResourceCache() *resourceCache {
	return &resourceCache{
		resources: map[string]*unstructured.Unstructured{},
	}
}

func (rc *resourceCache) Get(obj *unstructured.Unstructured) *unstructured.Unstructured {
	existing, exists := rc.resources[rc.objectKey(obj)]
	if !exists {
		return nil
	}

	return existing.DeepCopy()
}

func (rc *resourceCache) Set(obj *unstructured.Unstructured) {
	rc.resources[rc.objectKey(obj)] = obj.DeepCopy()
}

func (rc *resourceCache) Delete(obj *unstructured.Unstructured) {
	delete(rc.resources, rc.objectKey(obj))
}

func (rc *resourceCache) objectKey(obj *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s", obj.GetNamespace(), obj.GetName())
}

func main() {
	rootCtx := context.Background()

	opt := options{
		namespace:         "default",
		hideManagedFields: true,
	}

	pflag.StringVar(&opt.kubeconfig, "kubeconfig", opt.kubeconfig, "kubeconfig file to use (uses $KUBECONFIG by default)")
	pflag.StringVarP(&opt.namespace, "namespace", "n", opt.namespace, "Kubernetes namespace to watch resources in")
	pflag.StringVarP(&opt.labels, "labels", "l", opt.labels, "Label-selector as an alternative to specifying resource names")
	pflag.BoolVar(&opt.hideManagedFields, "hide-managed", opt.hideManagedFields, "Do not show managed fields")
	pflag.BoolVarP(&opt.verbose, "verbose", "v", opt.verbose, "Enable more verbose output")
	pflag.Parse()

	// setup logging
	var log = logrus.New()
	log.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: time.RFC1123,
	})

	if opt.verbose {
		log.SetLevel(logrus.DebugLevel)
	}

	// validate CLI flags
	if opt.kubeconfig == "" {
		opt.kubeconfig = os.Getenv("KUBECONFIG")
	}

	args := pflag.Args()
	if len(args) == 0 {
		log.Fatal("No resource kind and name given.")
	}

	resourceKinds := strings.Split(strings.ToLower(args[0]), ",")
	resourceNames := args[1:]

	// is there a label selector?
	if opt.labels != "" {
		selector, err := labels.Parse(opt.labels)
		if err != nil {
			log.Fatalf("Invalid label selector: %v", err)
		}

		opt.selector = selector
	}

	hasNames := len(resourceNames) > 0
	if hasNames && opt.selector != nil {
		log.Fatal("Cannot specify both resource names and a label selector at the same time.")
	}

	// setup kubernetes client
	config, err := clientcmd.BuildConfigFromFlags("", opt.kubeconfig)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes clientset: %v", err)
		fmt.Println(clientset)
	}

	log.Debug("Creating REST mapper...")

	mapper, err := getRESTMapper(config, log)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes REST mapper: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create dynamic Kubernetes client: %v", err)
	}

	// validate resource kinds
	log.Debug("Resolving resource kinds...")

	kinds := map[string]schema.GroupVersionKind{}
	for _, resourceKind := range resourceKinds {
		log.Debugf("Resolving %s...", resourceKind)

		gvk, err := mapper.KindFor(schema.GroupVersionResource{Resource: resourceKind})
		if err != nil {
			log.Fatalf("Unknown resource kind %q: %v", resourceKind, err)
		}

		kinds[gvk.String()] = gvk
	}

	// setup watches for each kind
	log.Debug("Starting to watch resources...")

	wg := sync.WaitGroup{}
	for _, gvk := range kinds {
		dynamicInterface, err := getDynamicInterface(gvk, opt.namespace, dynamicClient, mapper)
		if err != nil {
			log.Fatalf("Failed to create dynamic interface for %q resources: %v", gvk.Kind, err)
		}

		w, err := dynamicInterface.Watch(rootCtx, v1.ListOptions{
			LabelSelector: opt.labels,
		})
		if err != nil {
			log.Fatalf("Failed to create watch for %q resources: %v", gvk.Kind, err)
		}

		wg.Add(1)
		go func() {
			watcher(rootCtx, w, opt.hideManagedFields)
			wg.Done()
		}()
	}

	wg.Wait()
}

func watcher(ctx context.Context, w watch.Interface, hideManagedFields bool) {
	cache := newResourceCache()

	for event := range w.ResultChan() {
		metaObject, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		if hideManagedFields {
			metaObject.SetManagedFields(nil)
		}

		key := metaObject.GetName()
		if ns := metaObject.GetNamespace(); ns != "" {
			key = fmt.Sprintf("%s/%s", ns, key)
		}

		switch event.Type {
		case watch.Added:
			encoded, _ := yaml.Marshal(event.Object)
			fmt.Printf("--- CREATE --- %s ---------------------------------------------\n", key)
			fmt.Printf("%s\n\n", strings.TrimSpace(string(encoded)))
			cache.Set(metaObject)

		case watch.Modified:
			previousObject := cache.Get(metaObject)
			cache.Set(metaObject)

			fmt.Printf("--- UPDATE --- %s ---------------------------------------------\n", key)
			fmt.Printf("%s\n\n", strings.TrimSpace(diffObjects(previousObject, metaObject)))

		case watch.Deleted:
			cache.Delete(metaObject)
			// encoded, _ := yaml.Marshal(event.Object)
			fmt.Printf("--- DELETE --- %s ---------------------------------------------\n", key)
			// fmt.Printf("%s\n\n", strings.TrimSpace(string(encoded)))
		}
	}
}

func diffObjects(a, b *unstructured.Unstructured) string {
	encodedA, _ := yaml.Marshal(a)
	encodedB, _ := yaml.Marshal(b)

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(encodedA)),
		B:        difflib.SplitLines(string(encodedB)),
		FromFile: "Previous",
		ToFile:   "Current",
		Context:  3,
	}

	diffStr, _ := difflib.GetUnifiedDiffString(diff)

	return diffStr
}

func getRESTMapper(config *rest.Config, log logrus.FieldLogger) (meta.RESTMapper, error) {
	var discoveryClient discovery.DiscoveryInterface

	home, err := os.UserHomeDir()
	if err != nil {
		log.Warn("Cannot determine home directory, will disable discovery cache.")

		discoveryClient, err = discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			return nil, err
		}
	} else {
		cacheDir := filepath.Join(home, ".kube", "cache")

		discoveryClient, err = disk.NewCachedDiscoveryClientForConfig(config, cacheDir, cacheDir, 10*time.Minute)
		if err != nil {
			return nil, err
		}
	}

	cache := memory.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cache)
	fancyMapper := restmapper.NewShortcutExpander(mapper, discoveryClient)

	return fancyMapper, nil
}

func getDynamicInterface(gvk schema.GroupVersionKind, namespace string, dynamicClient dynamic.Interface, mapper meta.RESTMapper) (dynamic.ResourceInterface, error) {
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to determine mapping: %w", err)
	}

	namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace

	var dr dynamic.ResourceInterface
	if namespaced {
		// namespaced resources should specify the namespace
		dr = dynamicClient.Resource(mapping.Resource).Namespace(namespace)
	} else {
		// for cluster-wide resources
		dr = dynamicClient.Resource(mapping.Resource)
	}

	return dr, nil
}
