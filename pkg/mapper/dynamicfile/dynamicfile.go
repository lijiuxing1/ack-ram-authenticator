package dynamicfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AliyunContainerService/ack-ram-authenticator/pkg/arn"
	"github.com/AliyunContainerService/ack-ram-authenticator/pkg/config"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"
)

type DynamicFileMapStore struct {
	mutex sync.RWMutex
	users map[string]config.UserMapping
	roles map[string]config.RoleMapping
	// Used as set.
	aliAccounts map[string]interface{}
	filename    string
}

type DynamicFileData struct {
	// RoleMappings is a list of mappings from AlibabaCloud RAM Role to
	// Kubernetes username + groups.
	RoleMappings []config.RoleMapping `json:"mapRoles"`

	// UserMappings is a list of mappings from AlibabaCloud RAM User to
	// Kubernetes username + groups.
	UserMappings []config.UserMapping `json:"mapUsers"`
	// AutoMappedAlibabaCloudAccounts is a list of Alibaba Cloud accounts that are allowed without an explicit user/role mapping.
	// RAM ARN from these accounts automatically maps to the Kubernetes username.
	AutoMappedAlibabaCloudAccounts []string `json:"mapAccounts"`
}

type ErrParsingMap struct {
	errors []error
}

func (err ErrParsingMap) Error() string {
	return fmt.Sprintf("error parsing dynamic file: %v", err.errors)
}

func waitUntilFileAvailable(filename string) error {
	for {
		_, err := os.Stat(filename)
		if os.IsNotExist(err) {
			time.Sleep(1 * time.Second)
			continue
		} else {
			return err
		}
	}
}

func (m *DynamicFileMapStore) loadDynamicFile() error {
	err := waitUntilFileAvailable(m.filename)
	if err != nil {
		logrus.Errorf("LoadDynamicFile: failed to wait till dynamic file available %v", err)
		return err
	}
	logrus.Infof("LoadDynamicFile: %v is available. loading", m.filename)
	// load the initial file content into memory
	userMappings, roleMappings, aliAccounts, err := ParseMap(m.filename)
	if err != nil {
		logrus.Errorf("LoadDynamicFile: There was an error parsing the dynamic file: %+v. Map is not updated. Please correct dynamic file", err)
		return err
	} else {
		m.saveMap(userMappings, roleMappings, aliAccounts)
	}
	return nil
}

func NewDynamicFileMapStore(filename string) (*DynamicFileMapStore, error) {
	ms := DynamicFileMapStore{}
	ms.filename = filename
	return &ms, nil
}

func (m *DynamicFileMapStore) startLoadDynamicFile(stopCh <-chan struct{}) {
	go wait.Until(func() {
		m.loadDynamicFile()
		// start to watch the file change
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			logrus.Errorf("startLoadDynamicFile: failed when call fsnotify.NewWatcher, %+v", err)
		}
		err = watcher.Add(m.filename)
		if err != nil {
			logrus.Errorf("startLoadDynamicFile: could not add file to watcher %v", err)
		}

		defer watcher.Close()
		for {
			select {
			case <-stopCh:
				return
			case event := <-watcher.Events:
				switch {
				case event.Op&fsnotify.Write == fsnotify.Write, event.Op&fsnotify.Create == fsnotify.Create:
					// reload the access entry file
					logrus.Info("startLoadDynamicFile: got WRITE/CREATE event reload it the memory")
					m.loadDynamicFile()
				case event.Op&fsnotify.Rename == fsnotify.Rename, event.Op&fsnotify.Remove == fsnotify.Remove:
					logrus.Info("startLoadDynamicFile: got RENAME/REMOVE event")
					// test if the "REMOVE" is triggered by vi or cp cmd
					_, err := os.Stat(m.filename)
					if os.IsNotExist(err) {
						// the "REMOVE" event is  not triggered by vi or cp cmd
						// reset memory
						userMappings := make([]config.UserMapping, 0)
						roleMappings := make([]config.RoleMapping, 0)
						aliAccounts := make([]string, 0)
						m.saveMap(userMappings, roleMappings, aliAccounts)
					}
					return
				}
			case err := <-watcher.Errors:
				logrus.Errorf("startLoadDynamicFile: watcher.Errors for dynamic file %v", err)
			}
		}
	}, time.Second, stopCh)
}

func ParseMap(filename string) (userMappings []config.UserMapping, roleMappings []config.RoleMapping, aliAccounts []string, err error) {
	errs := make([]error, 0)
	userMappings = make([]config.UserMapping, 0)
	roleMappings = make([]config.RoleMapping, 0)

	dynamicContent, err := os.ReadFile(filename)
	if err != nil {
		logrus.Errorf("ParseMap: could not read from dynamic file")
		return userMappings, roleMappings, aliAccounts, err
	}

	var dynamicFileData DynamicFileData
	err = json.Unmarshal([]byte(dynamicContent), &dynamicFileData)
	if err != nil {
		if len(dynamicContent) == 0 {
			return userMappings, roleMappings, aliAccounts, nil
		}
		logrus.Error("ParseMap: could not unmarshal dynamic file.")
		return userMappings, roleMappings, aliAccounts, err
	}

	for _, userMapping := range dynamicFileData.UserMappings {
		if userMapping.UserARN == "" {
			errs = append(errs, fmt.Errorf("Value for userarn must be supplied"))
		} else {
			userMappings = append(userMappings, userMapping)
		}
	}

	for _, roleMapping := range dynamicFileData.RoleMappings {
		if roleMapping.RoleARN == "" {
			errs = append(errs, fmt.Errorf("Value for rolearn must be supplied"))
		} else {
			roleMappings = append(roleMappings, roleMapping)
		}
	}

	aliAccounts = dynamicFileData.AutoMappedAlibabaCloudAccounts[:]

	if len(errs) > 0 {
		logrus.Warnf("ParseMap: Errors parsing dynamic file: %+v", errs)
		err = ErrParsingMap{errors: errs}
	}
	return userMappings, roleMappings, aliAccounts, err
}
func (ms *DynamicFileMapStore) saveMap(
	userMappings []config.UserMapping,
	roleMappings []config.RoleMapping,
	aliAccounts []string) {

	ms.mutex.Lock()
	defer ms.mutex.Unlock()
	ms.users = make(map[string]config.UserMapping)
	ms.roles = make(map[string]config.RoleMapping)
	ms.aliAccounts = make(map[string]interface{})

	for _, user := range userMappings {
		canonicalizedARN, _ := arn.Canonicalize(strings.ToLower(user.UserARN))
		ms.users[canonicalizedARN] = user
	}
	for _, role := range roleMappings {
		canonicalizedARN, _ := arn.Canonicalize(strings.ToLower(role.RoleARN))
		ms.roles[canonicalizedARN] = role
	}
	for _, aliAccount := range aliAccounts {
		ms.aliAccounts[aliAccount] = nil
	}
}

// UserNotFound is the error returned when the user is not found in the config map.
var UserNotFound = errors.New("User not found in dynamic file")

// RoleNotFound is the error returned when the role is not found in the config map.
var RoleNotFound = errors.New("Role not found in dynamic file")

func (ms *DynamicFileMapStore) UserMapping(arn string) (config.UserMapping, error) {
	ms.mutex.RLock()
	defer ms.mutex.RUnlock()
	if user, ok := ms.users[arn]; !ok {
		return config.UserMapping{}, UserNotFound
	} else {
		return user, nil
	}
}

func (ms *DynamicFileMapStore) RoleMapping(arn string) (config.RoleMapping, error) {
	ms.mutex.RLock()
	defer ms.mutex.RUnlock()
	if role, ok := ms.roles[arn]; !ok {
		return config.RoleMapping{}, RoleNotFound
	} else {
		return role, nil
	}
}

func (ms *DynamicFileMapStore) AliAccount(id string) bool {
	ms.mutex.RLock()
	defer ms.mutex.RUnlock()
	_, ok := ms.aliAccounts[id]
	return ok
}

func (ms *DynamicFileMapStore) LogMapping() {
	ms.mutex.RLock()
	defer ms.mutex.RUnlock()
	for _, user := range ms.users {
		logrus.Info(user)
	}
	for _, role := range ms.roles {
		logrus.Info(role)
	}
	for aliAccount, _ := range ms.aliAccounts {
		logrus.Info(aliAccount)
	}
}
