package clientcert

import (
	"context"
	"crypto/x509/pkix"
	"fmt"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/operator/events"
	certificates "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	testinghelpers "open-cluster-management.io/registration/pkg/helpers/testing"
	"open-cluster-management.io/registration/pkg/hub/user"
)

const (
	testNamespace  = "testns"
	testAgentName  = "testagent"
	testSecretName = "testsecret"
	testCSRName    = "testcsr"
)

var commonName = fmt.Sprintf("%s%s:%s", user.SubjectPrefix, testinghelpers.TestManagedClusterName, testAgentName)

func TestSync(t *testing.T) {
	testSubject := &pkix.Name{
		CommonName: commonName,
	}

	cases := []struct {
		name                         string
		queueKey                     string
		secrets                      []runtime.Object
		approvedCSRCert              *testinghelpers.TestCert
		keyDataExpected              bool
		csrNameExpected              bool
		additonalSecretDataSensitive bool
		validateActions              func(t *testing.T, hubActions, agentActions []clienttesting.Action)
	}{
		{
			name:            "agent bootstrap",
			secrets:         []runtime.Object{},
			queueKey:        "key",
			keyDataExpected: true,
			csrNameExpected: true,
			validateActions: func(t *testing.T, hubActions, agentActions []clienttesting.Action) {
				testinghelpers.AssertActions(t, hubActions, "create")
				actual := hubActions[0].(clienttesting.CreateActionImpl).Object
				if _, ok := actual.(*unstructured.Unstructured); !ok {
					t.Errorf("expected csr was created, but failed")
				}
				testinghelpers.AssertActions(t, agentActions, "get")
			},
		},
		{
			name:     "syc csr after bootstrap",
			queueKey: testSecretName,
			secrets: []runtime.Object{
				testinghelpers.NewHubKubeconfigSecret(testNamespace, testSecretName, "1", nil, map[string][]byte{
					ClusterNameFile: []byte(testinghelpers.TestManagedClusterName),
					AgentNameFile:   []byte(testAgentName),
				},
				),
			},
			approvedCSRCert: testinghelpers.NewTestCert(commonName, 10*time.Second),
			validateActions: func(t *testing.T, hubActions, agentActions []clienttesting.Action) {
				testinghelpers.AssertActions(t, hubActions, "get", "get")
				testinghelpers.AssertActions(t, agentActions, "get", "update")
				actual := agentActions[1].(clienttesting.UpdateActionImpl).Object
				secret := actual.(*corev1.Secret)
				valid, err := IsCertificateValid(secret.Data[TLSCertFile], testSubject)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if !valid {
					t.Error("client certificate is invalid")
				}
			},
		},
		{
			name:     "sync a valid hub kubeconfig secret",
			queueKey: testSecretName,
			secrets: []runtime.Object{
				testinghelpers.NewHubKubeconfigSecret(testNamespace, testSecretName, "1", testinghelpers.NewTestCert(commonName, 10000*time.Second), map[string][]byte{
					ClusterNameFile: []byte(testinghelpers.TestManagedClusterName),
					AgentNameFile:   []byte(testAgentName),
					KubeconfigFile:  testinghelpers.NewKubeconfig(nil, nil),
				}),
			},
			validateActions: func(t *testing.T, hubActions, agentActions []clienttesting.Action) {
				testinghelpers.AssertNoActions(t, hubActions)
				testinghelpers.AssertActions(t, agentActions, "get")
			},
		},
		{
			name:     "sync an expiring hub kubeconfig secret",
			queueKey: testSecretName,
			secrets: []runtime.Object{
				testinghelpers.NewHubKubeconfigSecret(testNamespace, testSecretName, "1", testinghelpers.NewTestCert(commonName, -3*time.Second), map[string][]byte{
					ClusterNameFile: []byte(testinghelpers.TestManagedClusterName),
					AgentNameFile:   []byte(testAgentName),
					KubeconfigFile:  testinghelpers.NewKubeconfig(nil, nil),
				}),
			},
			keyDataExpected: true,
			csrNameExpected: true,
			validateActions: func(t *testing.T, hubActions, agentActions []clienttesting.Action) {
				testinghelpers.AssertActions(t, hubActions, "create")
				actual := hubActions[0].(clienttesting.CreateActionImpl).Object
				if _, ok := actual.(*unstructured.Unstructured); !ok {
					t.Errorf("expected csr was created, but failed")
				}
				testinghelpers.AssertActions(t, agentActions, "get")
			},
		},
		{
			name:     "sync when additional secret data changes",
			queueKey: testSecretName,
			secrets: []runtime.Object{
				testinghelpers.NewHubKubeconfigSecret(testNamespace, testSecretName, "1", testinghelpers.NewTestCert(commonName, 10000*time.Second), map[string][]byte{
					ClusterNameFile: []byte(testinghelpers.TestManagedClusterName),
					AgentNameFile:   []byte("invalid-name"),
				}),
			},
			keyDataExpected:              true,
			csrNameExpected:              true,
			additonalSecretDataSensitive: true,
			validateActions: func(t *testing.T, hubActions, agentActions []clienttesting.Action) {
				testinghelpers.AssertActions(t, hubActions, "create")
				actual := hubActions[0].(clienttesting.CreateActionImpl).Object
				if _, ok := actual.(*unstructured.Unstructured); !ok {
					t.Errorf("expected csr was created, but failed")
				}
				testinghelpers.AssertActions(t, agentActions, "get")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctrl := &mockCSRControl{}
			csrs := []runtime.Object{}
			if c.approvedCSRCert != nil {
				csr := testinghelpers.NewApprovedCSR(testinghelpers.CSRHolder{Name: testCSRName})
				csr.Status.Certificate = c.approvedCSRCert.Cert
				csrs = append(csrs, csr)
				ctrl.approved = true
				ctrl.issuedCertData = c.approvedCSRCert.Cert
			}
			hubKubeClient := kubefake.NewSimpleClientset(csrs...)
			ctrl.csrClient = &hubKubeClient.Fake

			// GenerateName is not working for fake clent, we set the name with prepend reactor
			hubKubeClient.PrependReactor(
				"create",
				"certificatesigningrequests",
				func(action clienttesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, testinghelpers.NewCSR(testinghelpers.CSRHolder{Name: testCSRName}), nil
				},
			)
			agentKubeClient := kubefake.NewSimpleClientset(c.secrets...)

			clientCertOption := ClientCertOption{
				SecretNamespace: testNamespace,
				SecretName:      testSecretName,
				AdditionalSecretData: map[string][]byte{
					ClusterNameFile: []byte(testinghelpers.TestManagedClusterName),
					AgentNameFile:   []byte(testAgentName),
				},
				AdditionalSecretDataSensitive: c.additonalSecretDataSensitive,
			}
			csrOption := CSROption{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-",
				},
				Subject:    testSubject,
				SignerName: certificates.KubeAPIServerClientSignerName,
			}

			controller := &clientCertificateController{
				ClientCertOption: clientCertOption,
				CSROption:        csrOption,
				csrControl:       ctrl,
				spokeCoreClient:  agentKubeClient.CoreV1(),
				controllerName:   "test-agent",
			}

			if c.approvedCSRCert != nil {
				controller.csrName = testCSRName
				controller.keyData = c.approvedCSRCert.Key
			}

			err := controller.sync(context.TODO(), testinghelpers.NewFakeSyncContext(t, c.queueKey))
			if err != nil {
				t.Errorf("unexpected error %v", err)
			}

			hasKeyData := controller.keyData != nil
			if c.keyDataExpected != hasKeyData {
				t.Error("controller.keyData should be set")
			}

			hasCSRName := controller.csrName != ""
			if c.csrNameExpected != hasCSRName {
				t.Error("controller.csrName should be set")
			}

			c.validateActions(t, hubKubeClient.Actions(), agentKubeClient.Actions())
		})
	}
}

var _ csrControl = &mockCSRControl{}

type mockCSRControl struct {
	approved       bool
	issuedCertData []byte
	csrClient      *clienttesting.Fake
}

func (m *mockCSRControl) create(ctx context.Context, recorder events.Recorder, objMeta metav1.ObjectMeta, csrData []byte, signerName string) (string, error) {
	mockCSR := &unstructured.Unstructured{}
	m.csrClient.Invokes(clienttesting.CreateActionImpl{
		ActionImpl: clienttesting.ActionImpl{
			Verb: "create",
		},
		Object: mockCSR,
	}, nil)
	return objMeta.Name + rand.String(4), nil
}

func (m *mockCSRControl) isApproved(name string) (bool, error) {
	m.csrClient.Invokes(clienttesting.GetActionImpl{
		ActionImpl: clienttesting.ActionImpl{
			Verb: "get",
		},
	}, nil)
	return m.approved, nil
}

func (m *mockCSRControl) getIssuedCertificate(name string) ([]byte, error) {
	m.csrClient.Invokes(clienttesting.GetActionImpl{
		ActionImpl: clienttesting.ActionImpl{
			Verb: "get",
		},
	}, nil)
	return m.issuedCertData, nil
}

func (m *mockCSRControl) informer() cache.SharedIndexInformer {
	panic("implement me")
}
