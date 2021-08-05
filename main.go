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
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"
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
}

func main() {
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "absolute path to the kubeconfig file")
	concurentNum := flag.Int("concurrent", 10, "number of concurrent clients")
	duration := flag.Int("duration", 10, "duration for running this test, in second")
	clean := flag.Bool("clean", false, "only do clean up operation")
	tmeplate := flag.String("template", "./manifestwork-template.yaml", "path to the template file")

	flag.Parse()

	logger := log.Log.WithName("etcd-replayer")

	wg := &sync.WaitGroup{}

	stop := make(chan struct{})

	w := &unstructured.Unstructured{}

	dat, err := ioutil.ReadFile(*tmeplate)
	if err != nil {
		logger.Error(err, "failed to read template")
		os.Exit(1)
	}

	if err := yaml.Unmarshal(dat, w); err != nil {
		logger.Error(err, "failed to parse template")
		os.Exit(1)
	}

	go func() {
		logger.Error(http.ListenAndServe("localhost:6060", nil), "pperf server")
	}()

	logger.Info(fmt.Sprintf("testing at %v(duration) seconds, %v(concurrent update client numbers) on clean == %v", *duration, *concurentNum, *clean))

	now := time.Now()
	for idx := 0; idx < *concurentNum; idx++ {
		idx := idx
		go NewRunner(
			WithNameSuffix(idx),
			WithTemplate(w),
			WithStop(stop),
			WithWaitGroup(wg),
			WithLogger(logger),
			WithKubePath(*kubeconfig),
		).run(*clean)

	}

	logger.Info(fmt.Sprintf("created %v templates in %v seconds", *concurentNum, time.Now().Sub(now).Seconds()))

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	dur := time.Duration(*duration) * time.Second
	timeout := time.After(dur)

	cleanUp := func() {
		close(stop)
	}

	defer wg.Wait()

	if *clean {
		return
	}

	select {
	case <-c:
		logger.Info("system interrupt")
	case <-timeout:
		logger.Info(fmt.Sprintf("stop after %v", time.Now().Sub(now).Seconds()))
	}

	cleanUp()
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
	name       string
	kubeconfig string
	client.Client
	template *unstructured.Unstructured
	stop     chan struct{}
	logger   logr.Logger
	wg       *sync.WaitGroup
}

func WithKubePath(kubeconfig string) Option {
	return func(r *Runner) {
		r.kubeconfig = kubeconfig
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

func (r *Runner) configClient() error {
	config, err := clientcmd.BuildConfigFromFlags("", r.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to load rest.Config, error: %w", err)
	}

	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 10
	t.MaxConnsPerHost = 10
	t.MaxIdleConnsPerHost = 10

	transportConfig, err := config.TransportConfig()
	if err != nil {
		return fmt.Errorf("failed to get TransportConfig, error: %w", err)

	}

	tlsConfig, err := transport.TLSConfigFor(transportConfig)
	if err != nil {
		return fmt.Errorf("%s failed to create tlsConfig, error: %w", r.name, err)
	}

	tlsConfig.InsecureSkipVerify = true

	t.TLSClientConfig = tlsConfig
	config.Transport = t

	// make sure the config TLSClientConfig won't override the custom Transport
	config.TLSClientConfig = restclient.TLSClientConfig{}

	config.QPS = 500.0
	config.Burst = 1000

	cl, err := client.New(config, client.Options{})
	if err != nil {
		return fmt.Errorf("failed to create tlsConfig, error: %w", err)
	}

	r.Client = cl

	return nil
}

func (r *Runner) run(cleanUp bool) {
	r.initial()

	if cleanUp {
		r.delete()
		return
	}

	go func() {
		r.wg.Add(1)

		r.update()

		r.wg.Done()
	}()
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

	return
}

func (r *Runner) create() error {
	ctx := context.TODO()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.template.GetNamespace(),
		},
	}

	if err := r.Client.Create(ctx, ns); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			r.logger.Error(err, "failed to create namespace")
			return err
		}

	}

	if err := r.Client.Create(ctx, r.template.DeepCopy()); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			r.logger.Error(err, fmt.Sprintf("failed to create manifestwork: %s ", r.getKey()))
			return err
		}
	}

	return nil
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
		if !k8serrors.IsNotFound(err) {
			r.logger.Error(err, fmt.Sprintf("failed to delete manifestwork: %s", r.getKey()))
			return
		}
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.template.GetNamespace(),
		},
	}

	if err := r.Client.Delete(ctx, ns); err != nil {
		if !k8serrors.IsNotFound(err) {
			r.logger.Error(err, "failed to delete namespace")
			return
		}
	}
}

func (r *Runner) update() {
	r.logger.Info(r.name)

	cnt := 0
	for err := r.configClient(); err != nil; err = r.configClient() {
		r.logger.Error(err, "failed to create client")
		time.Sleep(10 * time.Millisecond)

		cnt += 1
		if cnt == 30 {
			return
		}
	}

	if err := r.create(); err != nil {
		r.logger.Error(err, "failed to create resource")
		return
	}

	ctx := context.TODO()

	key := r.getKey()

	suffix := 1
	ticker := time.NewTicker(5 * time.Millisecond)

	defer func() {
		r.delete()
		ticker.Stop()
	}()

	for {
		select {
		case <-r.stop:
			r.logger.Info(fmt.Sprintf("stop and delete %s", r.name))
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
}
