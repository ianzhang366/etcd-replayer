package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ghodss/yaml"
	"github.com/go-logr/logr"
	uzap "go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"net/http"
	_ "net/http/pprof"
)

var (
	s = runtime.NewScheme()
)

func init() {
	log.SetLogger(zap.New(zap.RawZapOpts(uzap.AddCallerSkip(1)), zap.UseDevMode(true)))
	// SchemeGroupVersion is group version used to register these objects

}

func main() {
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file")
	concurentNum := flag.Int("n", 10, "number of concurrent clients")
	duration := flag.Int("d", 10, "duration for running this test, in second")
	path := "./manifestwork-template.yaml"

	flag.Parse()

	logger := log.Log.WithName("etcd")

	wg := &sync.WaitGroup{}

	stop := make(chan struct{})

	w := &unstructured.Unstructured{}

	dat, err := ioutil.ReadFile(path)
	if err != nil {
		logger.Error(err, "failed to read template")
		os.Exit(1)
	}

	if err := yaml.Unmarshal(dat, w); err != nil {
		logger.Error(err, "failed to parse template")
		os.Exit(1)
	}

	logger.Info(fmt.Sprintf("testing at %v(duration), %v(concurrent update client numbers)", *duration, *concurentNum))

	go func() {
		logger.Error(http.ListenAndServe("localhost:6060", nil), "pperf server")
	}()

	runners := []*Runner{}

	wg.Add(*concurentNum)

	now := time.Now()
	for idx := 0; idx < *concurentNum; idx++ {
		t := NewRunner(
			WithNameSuffix(idx),
			WithClient(*kubeconfig, logger),
			WithTemplate(w),
			WithStop(stop),
			WithWaitGroup(wg),
			WithLogger(logger))

		t.initial()
		t.create()

		runners = append(runners, t)
	}

	logger.Info(fmt.Sprintf("created %v templates in %v seconds", len(runners), time.Now().Sub(now).Seconds()))

	for _, r := range runners {
		go r.update()
	}

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	dur := time.Duration(*duration) * time.Second
	timeout := time.After(dur)

	defer func() {
		for _, r := range runners {
			r.delete()
		}
	}()

	select {
	case <-c:
		close(stop)
		logger.Info("system interrupt")
	case <-timeout:
		close(stop)
		logger.Info(fmt.Sprintf("stop after %v", time.Now().Sub(now).Seconds()))
	}

	wg.Wait()
}

type Option func(*Runner)

func NewRunner(ops ...Option) *Runner {
	r := &Runner{}

	for _, ops := range ops {
		ops(r)
	}

	return r
}

type Runner struct {
	name string
	client.Client
	template *unstructured.Unstructured
	stop     chan struct{}
	logger   logr.Logger
	wg       *sync.WaitGroup
}

func WithClient(kubeconfig string, logger logr.Logger) Option {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		logger.Error(err, "failed to load rest.Config")
		os.Exit(1)
	}

	cfg.QPS = 200.0
	cfg.Burst = 400

	cl, err := client.New(cfg, client.Options{})
	if err != nil {
		logger.Error(err, "failed to create client.Client")
		os.Exit(1)
	}

	return func(r *Runner) {
		r.Client = cl
	}
}

func WithTemplate(w *unstructured.Unstructured) Option {
	return func(r *Runner) {
		r.template = w.DeepCopy()
	}
}

func WithNameSuffix(s int) Option {
	return func(r *Runner) {
		r.name = fmt.Sprintf("%v", s)
	}
}

func WithLogger(logger logr.Logger) Option {
	return func(r *Runner) {
		r.logger = logger
	}
}

func WithWaitGroup(wg *sync.WaitGroup) Option {
	return func(r *Runner) {
		r.wg = wg
	}
}

func WithStop(stop chan struct{}) Option {
	return func(r *Runner) {
		r.stop = stop
	}
}

func (r *Runner) initial() {
	payload := r.template.DeepCopy()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%v", payload.GetName(), r.name),
		},
	}

	key := types.NamespacedName{
		Name:      fmt.Sprintf("%s-%v", payload.GetName(), r.name),
		Namespace: ns.Name,
	}

	payload.SetNamespace(key.Namespace)
	payload.SetName(key.Name)

	r.template = payload.DeepCopy()
}

func (r *Runner) create() {
	ctx := context.TODO()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.template.GetNamespace(),
		},
	}

	if err := r.Client.Create(ctx, ns); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			r.logger.Error(err, "failed to create namespace")
			return
		}

	}

	if err := r.Client.Create(ctx, r.template.DeepCopy()); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			r.logger.Error(err, fmt.Sprintf("failed to create manifestwork: %s ", r.getKey()))
			return
		}
	}

	//r.logger.Info(fmt.Sprintf("created %s", key))
}

func (r *Runner) getKey() types.NamespacedName {
	return types.NamespacedName{
		Name:      r.template.GetName(),
		Namespace: r.template.GetNamespace(),
	}

}

func (r *Runner) delete() {
	ctx := context.TODO()
	if err := r.Client.Delete(ctx, r.template.DeepCopy()); err != nil {
		r.logger.Error(err, fmt.Sprintf("failed to delete manifestwork: %s", r.getKey()))
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.template.GetNamespace(),
		},
	}
	if err := r.Client.Delete(ctx, ns); err != nil {
		r.logger.Error(err, "failed to delete namespace")
	}
}

func (r *Runner) update() {
	r.logger.Info(fmt.Sprintf("start to run %s", r.name))

	ctx := context.TODO()

	key := r.getKey()

	go func() {
		suffix := 1
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		defer func() {

			r.wg.Done()
		}()

		for {
			select {
			case <-r.stop:
				return

			case <-ticker.C:
				if err := r.Client.Get(ctx, key, r.template); err != nil {
					r.logger.Error(err, "failed to Get")
					continue
				}

				originalIns := r.template.DeepCopy()

				labels := r.template.GetLabels()

				if labels == nil {
					labels = map[string]string{}
				}

				// Update the ReplicaSet
				labels["hello"] = fmt.Sprintf("world-%v", suffix)
				suffix += 1

				r.template.SetLabels(labels)

				if err := r.Client.Patch(context.TODO(), r.template, client.MergeFrom(originalIns)); err != nil {
					r.logger.Error(err, "failed to update")
				}
			}
		}
	}()
}
