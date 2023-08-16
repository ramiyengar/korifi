package helpers

import (
	"fmt"
	"strconv"

	. "github.com/onsi/ginkgo/v2" //lint:ignore ST1001 this is a test file
	. "github.com/onsi/gomega"    //lint:ignore ST1001 this is a test file
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	controllerruntime "sigs.k8s.io/controller-runtime"
)

func getClientSet() *kubernetes.Clientset {
	GinkgoHelper()

	config, err := controllerruntime.GetConfig()
	Expect(err).NotTo(HaveOccurred())
	clientSet, err := kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())

	return clientSet
}

func GetClusterVersion() (int, int) {
	GinkgoHelper()

	serverVersion, err := getClientSet().ServerVersion()
	Expect(err).NotTo(HaveOccurred())

	majorVersion, err := strconv.Atoi(serverVersion.Major)
	if err != nil {
		Skip(fmt.Sprintf("cannot determine kubernetes server major version: %v", err))
	}

	minorVersion, err := strconv.Atoi(serverVersion.Minor)
	if err != nil {
		Skip(fmt.Sprintf("cannot determine kubernetes server minor version: %v", err))
	}

	return majorVersion, minorVersion
}

func AddUserToKubeConfig(userName, userToken string) {
	GinkgoHelper()

	authInfo := clientcmdapi.NewAuthInfo()
	authInfo.Token = userToken

	configAccess := clientcmd.NewDefaultPathOptions()
	config, err := configAccess.GetStartingConfig()
	Expect(err).NotTo(HaveOccurred())
	config.AuthInfos[userName] = authInfo

	Expect(clientcmd.ModifyConfig(configAccess, *config, false)).To(Succeed())
}

func RemoveUserFromKubeConfig(userName string) {
	configAccess := clientcmd.NewDefaultPathOptions()
	config, err := configAccess.GetStartingConfig()
	Expect(err).NotTo(HaveOccurred())
	delete(config.AuthInfos, userName)

	Expect(clientcmd.ModifyConfig(configAccess, *config, false)).To(Succeed())
}
