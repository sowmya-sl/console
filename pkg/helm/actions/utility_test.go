package actions

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestGetSecret(t *testing.T) {
	releaseSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.example.v2",
			Namespace: "test-namespace",
			Labels:    map[string]string{"owner": "helm", "name": "example", "version": "2"},
		},
		Data: map[string][]byte{"release": []byte("encoded-release")},
	}

	authSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-secret", Namespace: "test-namespace"},
		Data:       map[string][]byte{"username": []byte("user"), "password": []byte("password")},
	}

	oldRevision := releaseSecret.DeepCopy()
	oldRevision.Name = "sh.helm.release.v1.example.v1"
	oldRevision.Labels["version"] = "1"

	client := k8sfake.NewSimpleClientset(authSecret, oldRevision, releaseSecret)

	secret, err := getSecret("test-namespace", "example", 2, client.CoreV1())
	require.NoError(t, err)
	require.Equal(t, *releaseSecret, secret)
}

func TestGetSecretWatchEvents(t *testing.T) {
	releaseSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1.example.v2",
			Namespace: "test-namespace",
			Labels:    map[string]string{"owner": "helm", "name": "example", "version": "2"},
		},
		Data: map[string][]byte{"release": []byte("encoded-release")},
	}

	errorSecret := releaseSecret.DeepCopy()
	errorSecret.Name = "example"
	errorSecret.Data = map[string][]byte{"error": []byte("installation failed")}

	otherRelease := releaseSecret.DeepCopy()
	otherRelease.Labels["name"] = "other-release"

	otherOwner := releaseSecret.DeepCopy()
	otherOwner.Labels["owner"] = "other-owner"

	otherNamespace := releaseSecret.DeepCopy()
	otherNamespace.Namespace = "other-namespace"

	tests := []struct {
		name        string
		objects     []runtime.Object
		events      []watch.Event
		watchErr    error
		closeWatch  bool
		expectedErr string
	}{
		{
			name: "ignore unrelated and malformed events before a matching modification",
			events: []watch.Event{
				{Type: watch.Bookmark, Object: &metav1.PartialObjectMetadata{}},
				{Type: watch.Deleted, Object: releaseSecret},
				{Type: watch.Added, Object: &v1.ConfigMap{}},
				{Type: watch.Added},
				{Type: watch.Added, Object: (*v1.Secret)(nil)},
				{Type: watch.Added, Object: otherRelease},
				{Type: watch.Added, Object: otherOwner},
				{Type: watch.Added, Object: otherNamespace},
				{Type: watch.Modified, Object: releaseSecret},
			},
		},
		{
			name:        "return and clean up an action error",
			objects:     []runtime.Object{errorSecret},
			events:      []watch.Event{{Type: watch.Added, Object: errorSecret}},
			expectedErr: "action error: installation failed",
		},
		{
			name: "return a watch error",
			events: []watch.Event{{
				Type: watch.Error,
				Object: &metav1.Status{
					Status:  metav1.StatusFailure,
					Reason:  metav1.StatusReasonForbidden,
					Code:    403,
					Message: "watch forbidden",
				},
			}},
			expectedErr: "watch forbidden",
		},
		{
			name:        "return a watch creation error",
			watchErr:    apierrors.NewForbidden(v1.Resource("secrets"), "", nil),
			expectedErr: "forbidden",
		},
		{
			name:        "return when the watch closes",
			closeWatch:  true,
			expectedErr: "not found: watch closed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			watcher := watch.NewRaceFreeFake()
			t.Cleanup(watcher.Stop)
			for _, event := range tt.events {
				watcher.Action(event.Type, event.Object)
			}
			if tt.closeWatch {
				watcher.Stop()
			}

			client := k8sfake.NewSimpleClientset(tt.objects...)
			client.PrependWatchReactor("secrets", k8stesting.DefaultWatchReactor(watcher, tt.watchErr))

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			secret, err := getSecretWithContext(ctx, "test-namespace", "example", 2, client.CoreV1())
			if tt.expectedErr != "" {
				require.ErrorContains(t, err, tt.expectedErr)
				require.Empty(t, secret)
			} else {
				require.NoError(t, err)
				require.Equal(t, *releaseSecret, secret)
			}

			if tt.watchErr == nil {
				require.True(t, watcher.IsStopped())
			}

			// Since only the error object is seeded and also removed in getSecret,
			// checking that there are no more secrets checks all test cases properly, actually.
			secrets, err := client.CoreV1().Secrets("test-namespace").List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Empty(t, secrets.Items)
		})
	}
}

func TestGetChartInfoFromIndexEntry(t *testing.T) {
	tests := []struct {
		name        string
		indexEntry  string
		namespace   string
		url         string
		wantName    string
		wantRepo    string
		wantVersion string
		wantErr     string
	}{
		{
			name:        "valid chartName--repoName http archive",
			indexEntry:  "influxdb--without-tls",
			namespace:   "default",
			url:         "https://example.com/influxdb-1.2.3.tgz",
			wantName:    "influxdb",
			wantRepo:    "without-tls",
			wantVersion: "1.2.3",
		},
		{
			name:        "oci registry artifact tag",
			indexEntry:  "mychart--oci-repo",
			namespace:   "default",
			url:         "oci://registry.example.com/helm-charts/mychart:0.1.0",
			wantName:    "mychart",
			wantRepo:    "oci-repo",
			wantVersion: "0.1.0",
		},
		{
			name:        "oci registry artifact with host port",
			indexEntry:  "mychart--local-oci",
			namespace:   "ns1",
			url:         "oci://localhost:5000/helm-charts/mychart:1.2.3",
			wantName:    "mychart",
			wantRepo:    "local-oci",
			wantVersion: "1.2.3",
		},
		{
			name:       "empty index entry",
			indexEntry: "",
			namespace:  "default",
			url:        "oci://registry.example.com/charts/mychart:1.0.0",
			wantErr:    "invalid index entry",
		},
		{
			name:       "missing separator",
			indexEntry: "influxdb",
			namespace:  "default",
			url:        "https://example.com/chart.tgz",
			wantErr:    "invalid index entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := getChartInfoFromIndexEntry(tt.indexEntry, tt.namespace, tt.url)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.Nil(t, info)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantName, info.Name)
			require.Equal(t, tt.wantRepo, info.RepositoryName)
			require.Equal(t, tt.namespace, info.RepositoryNamespace)
			require.Equal(t, tt.wantVersion, info.Version)
		})
	}
}

func TestGetSecretWatchDeadline(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	watcher := watch.NewRaceFreeFake()
	t.Cleanup(watcher.Stop)
	client.PrependWatchReactor("secrets", k8stesting.DefaultWatchReactor(watcher, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	// The fake watch stays open, so the context must end the wait.
	secret, err := getSecretWithContext(ctx, "test-namespace", "example", 2, client.CoreV1())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, secret)
	require.True(t, watcher.IsStopped())
}
