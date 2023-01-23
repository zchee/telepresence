package trafficmgr

import (
	"context"
	"github.com/datawire/k8sapi/pkg/k8sapi"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
	rpc "github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/scout"
	"github.com/telepresenceio/telepresence/v2/pkg/client/userd"
	mock_userd "github.com/telepresenceio/telepresence/v2/pkg/client/userd/mocks"
	mock_trafficmgr "github.com/telepresenceio/telepresence/v2/pkg/client/userd/trafficmgr/mocks"
	"google.golang.org/grpc"
	empty "google.golang.org/protobuf/types/known/emptypb"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	stduser "os/user"
	"testing"
	"time"
)

const (
	installID = "074469a9-bd46-42ea-b448-6a3df3a59609"
)

//go:generate mockgen -package=mock_trafficmgr -destination=mocks/kubeinterface_mock.go k8s.io/client-go/kubernetes Interface
//go:generate mockgen -package=mock_trafficmgr -destination=mocks/managerclient_mock.go github.com/telepresenceio/telepresence/rpc/v2/manager ManagerClient
type suiteSession struct {
	suite.Suite

	ctrl *gomock.Controller

	sessionBuilder *SessionBuilder

	reporter           *mock_userd.MockReporter
	kubeConfigResolver *mock_userd.MockKubeConfigResolver
	userService        *mock_userd.MockService

	kubernetesInterface      *mock_trafficmgr.MockInterface
	PortForwardDialerBuilder *mock_trafficmgr.MockPortForwardDialerBuilder
	managerConnector         *mock_trafficmgr.MockManagerConnector
	managerClient            *mock_trafficmgr.MockManagerClient
	userSessionCache         *mock_trafficmgr.MockUserSessionCache

	stdUser *mock_trafficmgr.MockUser
	stdOS   *mock_trafficmgr.MockOS
}

func (s *suiteSession) SetupTest() {
	s.ctrl = gomock.NewController(s.T())

	s.reporter = mock_userd.NewMockReporter(s.ctrl)
	s.kubeConfigResolver = mock_userd.NewMockKubeConfigResolver(s.ctrl)
	s.userService = mock_userd.NewMockService(s.ctrl)

	s.kubernetesInterface = mock_trafficmgr.NewMockInterface(s.ctrl)
	s.PortForwardDialerBuilder = mock_trafficmgr.NewMockPortForwardDialerBuilder(s.ctrl)
	s.managerConnector = mock_trafficmgr.NewMockManagerConnector(s.ctrl)
	s.managerClient = mock_trafficmgr.NewMockManagerClient(s.ctrl)
	s.userSessionCache = mock_trafficmgr.NewMockUserSessionCache(s.ctrl)

	s.stdUser = mock_trafficmgr.NewMockUser(s.ctrl)
	s.stdOS = mock_trafficmgr.NewMockOS(s.ctrl)

	s.sessionBuilder = &SessionBuilder{
		kcr:  s.kubeConfigResolver,
		p:    s.PortForwardDialerBuilder,
		mc:   s.managerConnector,
		sc:   s.userSessionCache,
		os:   s.stdOS,
		user: s.stdUser,
	}

	s.stdUser.EXPECT().Current().Return(&stduser.User{
		Uid:      "1000",
		Gid:      "1000",
		Username: "jdoe",
		Name:     "John doe",
		HomeDir:  "/home/jdoe",
	}, nil).AnyTimes()

	s.stdOS.EXPECT().Hostname().Return("localhost", nil).AnyTimes()
}

func (s *suiteSession) AfterTest(suiteName, testName string) {
	s.ctrl.Finish()
}

func (s *suiteSession) contextWithEnv(ctx context.Context) context.Context {
	return client.WithEnv(ctx, &client.Env{})
}

func (s *suiteSession) contextWithUserService(ctx context.Context) context.Context {
	return userd.WithService(ctx, s.userService)
}

func (s *suiteSession) contextWithClientConfig(ctx context.Context) context.Context {
	return client.WithConfig(ctx, &client.Config{
		Timeouts: client.Timeouts{
			PrivateClusterConnect:        60 * time.Minute,
			PrivateTrafficManagerConnect: 60 * time.Minute,
		},
		LogLevels:       client.LogLevels{},
		Images:          client.Images{},
		Cloud:           client.Cloud{},
		Grpc:            client.Grpc{},
		TelepresenceAPI: client.TelepresenceAPI{},
		Intercept:       client.Intercept{},
	})
}

func (s *suiteSession) ExpectNewKubeConfig(ctx context.Context, cr *rpc.ConnectRequest) {
	s.kubeConfigResolver.EXPECT().NewKubeconfig(ctx, cr.KubeFlags).Return(&client.Kubeconfig{
		KubeconfigExtension: client.KubeconfigExtension{
			Manager: &client.ManagerConfig{
				Namespace: "ambassador",
			},
		},
		Namespace:   "ambassador",
		Context:     "my-context",
		Server:      "https://my-context.cluster.bakerstreet.io",
		FlagMap:     nil,
		ConfigFlags: &genericclioptions.ConfigFlags{},
		RestConfig: &rest.Config{
			ContentConfig:   rest.ContentConfig{},
			Impersonate:     rest.ImpersonationConfig{},
			TLSClientConfig: rest.TLSClientConfig{},
		},
	}, nil)
}

func (s *suiteSession) buildContext() context.Context {
	ctx := context.Background()
	ctx = s.contextWithEnv(ctx)
	ctx = s.contextWithClientConfig(ctx)
	ctx = s.contextWithUserService(ctx)
	return k8sapi.WithK8sInterface(ctx, s.kubernetesInterface)
}

func (s *suiteSession) TestFirstNewSession() {
	// given
	ctx := s.buildContext()
	cr := &rpc.ConnectRequest{
		KubeFlags:   map[string]string{},
		IsPodDaemon: false,
	}

	s.reporter.EXPECT().Report(gomock.Any(), "connect")
	// Configure NewKubeConfig mock call.
	s.ExpectNewKubeConfig(ctx, cr)
	// Report set cluster id.
	s.reporter.EXPECT().SetMetadatum(gomock.Any(), "cluster_id", gomock.Any())
	// Report traffic manager connection since it's not a pod daemon
	s.reporter.EXPECT().Report(gomock.Any(), "connecting_traffic_manager", scout.Entry{
		Key:   "mapped_namespaces",
		Value: 0,
	})
	// Emulate expected port-forward with the cluster.
	dialer := mock_trafficmgr.NewMockPortForwardDialer(s.ctrl)
	s.PortForwardDialerBuilder.EXPECT().NewK8sPortForwardDialer(gomock.Any(), gomock.Any(), gomock.Any()).Return(
		dialer, nil)
	// Connect to the manager.
	s.managerConnector.EXPECT().
		Connect(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&grpc.ClientConn{}, s.managerClient, &manager.VersionInfo2{
			Name:    "",
			Version: "v3.10.4",
		}, nil)

	// Session info is not in cache
	s.userSessionCache.EXPECT().LoadSessionInfoFromUserCache(gomock.Any(), gomock.Any()).Return(nil, nil)
	// Connect then to get it.
	newSessionInfo := &manager.SessionInfo{
		SessionId: "",
		ClusterId: "",
		InstallId: nil,
	}
	s.managerClient.EXPECT().ArriveAsClient(gomock.Any(), &manager.ClientInfo{
		Name:      "jdoe@localhost",
		InstallId: installID,
		Product:   "telepresence",
		Version:   "v0.0.0-unknown",
	}).Return(newSessionInfo, nil)
	// Set the new session info in cache
	s.userSessionCache.EXPECT().SaveSessionInfoToUserCache(gomock.Any(), gomock.Any(), newSessionInfo).Return(nil)

	// Set manager client into the user service
	s.userService.EXPECT().SetManagerClient(s.managerClient)
	s.reporter.EXPECT().InstallID().Return(installID)
	s.managerClient.EXPECT().GetClientConfig(gomock.Any(), &empty.Empty{}).Return(&manager.CLIConfig{}, nil)

	// Report the end of the traffic manager connection
	s.reporter.EXPECT().Report(gomock.Any(), "finished_connecting_traffic_manager", gomock.Any())

	// when
	newCtx, sess, c := s.sessionBuilder.NewSession(ctx, s.reporter, cr)

	// then
	assert.NotNil(s.T(), newCtx)
	assert.NotNil(s.T(), sess)
	assert.NotNil(s.T(), c)
}

func TestSuiteSession(t *testing.T) {
	suite.Run(t, new(suiteSession))
}
