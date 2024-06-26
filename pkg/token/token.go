/*
Copyright 2017 by the contributors.

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

package token

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/AliyunContainerService/ack-ram-authenticator/pkg/arn"
	"github.com/AliyunContainerService/ack-ram-authenticator/pkg/utils"
	"github.com/AliyunContainerService/ack-ram-tool/pkg/credentials/provider"
	openapi "github.com/alibabacloud-go/darabonba-openapi/client"
	sts "github.com/alibabacloud-go/sts-20150401/client"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/aliyun/alibaba-cloud-sdk-go/sdk/responses"
	"github.com/aliyun/credentials-go/credentials"
	"github.com/satori/go.uuid"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientauthv1alpha1 "k8s.io/client-go/pkg/apis/clientauthentication/v1alpha1"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Identity is returned on successful Verify() results. It contains a parsed
// version of the ACK identity used to create the token.
type Identity struct {
	// ARN is the raw RAM Resource Name returned by sts:GetCallerIdentity
	ARN string

	// CanonicalARN is the RAM Resource Name converted to a more canonical
	// representation. In particular, STS assumed role ARNs like
	// "acs:ram::ACCOUNTID:assumed-role/ROLENAME/SESSIONNAME" are converted
	// to their RAM ARN equivalent "acs:ram::ACCOUNTID:role/NAME"
	CanonicalARN string

	// AccountID is the 16 digit RAM account number.
	AccountID string

	// UserID is the unique user/role ID (e.g., "AROAAAAAAAAAAAAAAAAAA").
	UserID string

	// SessionName is the STS session name (or "" if this is not a
	// session-based identity). For ECS instance roles, this will be the ECS
	// instance ID (e.g., "iZj6c792gcdoonnp1rd5y8Z"). You should only rely on it
	// if you trust that _only_ ECS is allowed to assume the RAM Role. If RAM
	// users or other roles are allowed to assume the role, they can provide
	// (nearly) arbitrary strings here.
	SessionName string

	// The Alibaba Cloud Access Key ID used to authenticate the request.  This can be used
	// in conjunction with CloudTrail to determine the identity of the individual
	// if the individual assumed an RAM role before making the request.
	AccessKeyID string
}

const (
	// The actual token expiration (presigned STS urls are valid for 15 minutes after timestamp in query param Timestamp).
	presignedURLExpiration = 15 * time.Minute
	v1Prefix               = "k8s-ack-v1."
	v2Prefix               = "k8s-ack-v2."
	maxTokenLenBytes       = 1024 * 4
	hostRegexp             = `^sts(\.[a-z1-9\-]+)?\.aliyuncs\.com(\.cn)?$`
	stsSignVersion         = "1.0"
	stsAPIVersion          = "2015-04-01"
	stsHost                = "https://sts.aliyuncs.com/"
	timeFormat             = "2006-01-02T15:04:05Z"
	respBodyFormat         = "JSON"
	percentEncode          = "%2F"
	httpGet                = "GET"
	ramRoleARNAuthType     = "ram_role_arn"
	defaultSTSEndpoint     = "sts.aliyuncs.com"
	vpcStsEndpoint         = "https://sts-vpc.%s.aliyuncs.com"
	defaultSTSProtocol     = "https"
	defaultRoleSessionName = "ack-ram-authenticator"
)

// Token is generated and used by Kubernetes client-go to authenticate with a Kubernetes cluster.
type Token struct {
	Token      string
	Expiration time.Time
}

// GetTokenOptions is passed to GetWithOptions to provide an extensible get token interface
type GetTokenOptions struct {
	Region        string
	ClusterID     string
	AssumeRoleARN string
}

// FormatError is returned when there is a problem with token that is
// an encoded sts request.  This can include the url, data, action or anything
// else that prevents the sts call from being made.
type FormatError struct {
	message string
}

func (e FormatError) Error() string {
	return "input token was not properly formatted: " + e.message
}

// STSError is returned when there was either an error calling STS or a problem
// processing the data returned from STS.
type STSError struct {
	message     string
	raiseToUser bool
}

func (e STSError) RaiseToUser() bool {
	return e.raiseToUser
}

func (e STSError) RawMessage() string {
	return e.message
}

func (e STSError) Error() string {
	return "sts getCallerIdentity failed: " + e.message
}

// NewSTSError creates a error of type STS.
func NewSTSError(m string) STSError {
	return STSError{message: m}
}

var parameterWhitelist = map[string]bool{
	"action":           true,
	"durationseconds":  true,
	"signatureversion": true,
	"signaturenonce":   true,
	"signaturemethod":  true,
	"accesskeyid":      true,
	"timestamp":        true,
	"signature":        true,
	"format":           true,
	"version":          true,
	"rolesessionname":  true,
	"rolearn":          true,
	"securitytoken":    true,
	"clusterid":        true,

	// v2
	"x-acs-action":          true,
	"x-acs-version":         true,
	"authorization":         true,
	"x-acs-signature-nonce": true,
	"x-acs-date":            true,
	"x-acs-content-sha256":  true,
	"x-acs-content-sm3":     true,
	"x-acs-security-token":  true,
	"ackclusterid":          true,
}

type getCallerIdentityWrapper struct {
	*responses.BaseResponse
	AccountID    string `json:"AccountId" xml:"AccountId"`
	UserID       string `json:"UserId" xml:"UserId"`
	RoleID       string `json:"RoleId" xml:"RoleId"`
	Arn          string `json:"Arn" xml:"Arn"`
	IdentityType string `json:"IdentityType" xml:"IdentityType"`
	PrincipalID  string `json:"PrincipalId" xml:"PrincipalId"`
	RequestID    string `json:"RequestId" xml:"RequestId"`
}

type acsCredentials struct {
	AccessKeyID         string `json:"AcsAccessKeyId"`
	AccessKeySecret     string `json:"AcsAccessKeySecret"`
	AccessSecurityToken string `json:"AcsAccessSecurityToken"`
}

// JSONStruct struct
type JSONStruct struct {
}

// Generator provides new tokens for the authenticator.
type Generator interface {
	// Get a token using credentials in the default credentials chain.
	Get(string) (Token, error)
	// GetWithRole creates a token by assuming the provided role, using the credentials in the default chain.
	GetWithRole(clusterID, roleARN string) (Token, error)
	// Get a token using the provided options
	GetWithOptions(options *GetTokenOptions) (Token, error)
	// GetWithSTS returns a token valid for clusterID using the given STS client.
	GetWithSTS(clusterID string, stsClient *sts.Client) (Token, error)
	// FormatJSON returns the client auth formatted json for the ExecCredential auth
	FormatJSON(Token) string
}

type generator struct {
	cache bool
}

// NewGenerator creates a Generator and returns it.
func NewGenerator() (Generator, error) {
	return generator{}, nil
}

// Get uses the directly available RAM credentials to return a token valid for
// clusterID. It follows the default RAM credential handling behavior.
func (g generator) Get(clusterID string) (Token, error) {
	return g.GetWithRole(clusterID, "")
}

// StdinStderrTokenProvider func
func StdinStderrTokenProvider() (string, error) {
	var v string
	fmt.Fprint(os.Stderr, "Assume Role MFA token code: ")
	_, err := fmt.Scanln(&v)
	return v, err
}

func (g generator) GetWithRole(clusterID string, roleARN string) (Token, error) {
	return g.GetWithOptions(&GetTokenOptions{
		ClusterID:     clusterID,
		AssumeRoleARN: roleARN,
	})
}

// GetWithOptions takes a GetTokenOptions struct, builds the STS client, and wraps GetWithSTS.
// If no session has been passed in options, it will build a new session. If an
// AssumeRoleARN was passed in then assume the role for the session.
func (g generator) GetWithOptions(options *GetTokenOptions) (Token, error) {
	if options.ClusterID == "" {
		return Token{}, fmt.Errorf("ClusterID is required")
	}

	cred, err := credentials.NewCredential(nil)
	if err != nil {
		return Token{}, fmt.Errorf("could not init credentials: %v", err)
	}
	//if tea.StringValue(cred.GetType()) == "ecs_ram_role" {
	//	return Token{}, fmt.Errorf("empty credentials given")
	//}
	//init sts client

	region := options.Region
	if region == "" {
		log.Warnf("empty region id given")
		region = utils.GetMetaData(utils.RegionID)
	}
	stsEndpoint := defaultSTSEndpoint
	if region != "" {
		stsEndpoint = fmt.Sprintf(vpcStsEndpoint, region)
	}

	stsAPI, err := sts.NewClient(&openapi.Config{
		Endpoint:   tea.String(stsEndpoint),
		Protocol:   tea.String(defaultSTSProtocol),
		Credential: cred,
	})

	if g.cache {
		// figure out what profile we're using
		var profile string
		if v := os.Getenv("ALIBABA_CLOUD_CREDENTIALS_PROFILE"); len(v) > 0 {
			profile = v
		} else {
			profile = "default"
		}

		profileConfig := getRamRoleArnProfile(profile)
		if profileConfig != nil {
			// create a cacheing Provider wrapper around the Credentials
			if cacheProvider, err := NewFileCacheProvider(options.ClusterID, profile, options.AssumeRoleARN, stsEndpoint, profileConfig); err == nil {
				stsAPI.Credential = &FileCacheCredential{&cacheProvider}
			}
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "unable to use cache: %v\n", err)
		}
	}
	// if a roleARN was specified, replace the STS client with one that uses
	// temporary credentials from that role.
	if options.AssumeRoleARN != "" {
		stsReq := &sts.AssumeRoleRequest{
			RoleArn:         tea.String(options.AssumeRoleARN),
			RoleSessionName: tea.String(fmt.Sprintf("%s-%d", defaultRoleSessionName, time.Now().UnixNano())),
		}
		assumeRes, err := stsAPI.AssumeRole(stsReq)
		if err != nil {
			return Token{}, fmt.Errorf("failed to assume ram role %s, err %v", options.AssumeRoleARN, err)
		}
		config := new(credentials.Config)
		config.RoleName = tea.String(options.AssumeRoleARN)
		config.AccessKeyId = assumeRes.Body.Credentials.AccessKeyId
		config.AccessKeySecret = assumeRes.Body.Credentials.AccessKeySecret
		config.SecurityToken = assumeRes.Body.Credentials.SecurityToken
		expiration, err := strconv.Atoi(tea.StringValue(assumeRes.Body.Credentials.Expiration))
		if err != nil {
			return Token{}, fmt.Errorf("failed to parse assumed credential expiration %s, err %v", tea.StringValue(assumeRes.Body.Credentials.Expiration), err)
		}
		config.RoleSessionExpiration = tea.Int(expiration)
		stsCred, err := credentials.NewCredential(config)
		if err != nil {
			return Token{}, fmt.Errorf("failed to init sts credential for role %s, err %v", options.AssumeRoleARN, err)
		}
		stsAPI.Credential = stsCred
	}

	return g.GetWithSTS(options.ClusterID, stsAPI)
}

// FormatJSON formats the json to support ExecCredential authentication
func (g generator) FormatJSON(token Token) string {
	expirationTimestamp := metav1.NewTime(token.Expiration)
	execInput := &clientauthv1alpha1.ExecCredential{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "client.authentication.k8s.io/v1beta1",
			Kind:       "ExecCredential",
		},
		Status: &clientauthv1alpha1.ExecCredentialStatus{
			ExpirationTimestamp: &expirationTimestamp,
			Token:               token.Token,
		},
	}
	enc, _ := json.Marshal(execInput)
	return string(enc)
}

// Verifier validates tokens by calling STS and returning the associated identity.
type Verifier interface {
	Verify(token string) (*Identity, error)
}

type tokenVerifier struct {
	client      *http.Client
	clusterID   string
	stsEndpoint string
}

func (v tokenVerifier) getClusterID() string {
	return v.clusterID
}

// NewVerifier creates a Verifier that is bound to the clusterID and uses the default http client.
func NewVerifier(region, clusterID string) Verifier {
	endpoint := provider.GetSTSEndpoint(region, true)
	if region == "" {
		endpoint = provider.GetSTSEndpoint(region, false)
	}
	log.Warnf("will use %s as sts endpoint", endpoint)

	rt := http.DefaultTransport.(*http.Transport).Clone()
	if v, err := strconv.Atoi(os.Getenv("STS_MAX_IDLE_CONNS_PER_HOST")); err == nil && v > 1 {
		rt.MaxIdleConnsPerHost = v
	} else {
		rt.MaxIdleConnsPerHost = 5
	}
	log.Warnf("will use %d as value of MaxIdleConnsPerHost", rt.MaxIdleConnsPerHost)

	client := &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return tokenVerifier{
		client:      client,
		clusterID:   clusterID,
		stsEndpoint: endpoint,
	}
}

// verify a sts host
func (v tokenVerifier) verifyHost(host string) error {
	if match, _ := regexp.MatchString(hostRegexp, host); !match {
		return FormatError{fmt.Sprintf("unexpected hostname %q in pre-signed URL", host)}
	}

	return nil
}

// verify a sts host
func (v tokenVerifier) verifyClusterID(clusterID string) error {
	if v.clusterID != clusterID {
		return FormatError{fmt.Sprintf("unexpected clusterid %s in token", clusterID)}
	}

	return nil
}

// Verify a token is valid for the specified clusterID. On success, returns an
// Identity that contains information about the RAM principal that created the
// token. On failure, returns nil and a non-nil error.
func (v tokenVerifier) Verify(token string) (*Identity, error) {
	if len(token) > maxTokenLenBytes {
		return nil, FormatError{"token is too large"}
	}

	if !strings.HasPrefix(token, v1Prefix) && !strings.HasPrefix(token, v2Prefix) {
		return nil, FormatError{"token is missing expected prefix"}
	}

	// TODO: this may need to be a constant-time base64 decoding
	tokenBytes, err := base64.StdEncoding.DecodeString(
		strings.TrimPrefix(strings.TrimPrefix(token, v1Prefix), v2Prefix),
	)
	if err != nil {
		return nil, FormatError{err.Error()}
	}

	var req *http.Request
	var accessKeyId string
	switch {
	case strings.HasPrefix(token, v2Prefix):
		log.Infof("start to parse token with prefix %s", v2Prefix)
		accessKeyId, req, err = v.parseV2Token(string(tokenBytes))
		if err != nil {
			return nil, FormatError{err.Error()}
		}
	case strings.HasPrefix(token, v1Prefix):
		log.Infof("start to parse token with prefix %s", v1Prefix)
		parsedURL, err := url.Parse(string(tokenBytes))
		if err != nil {
			return nil, FormatError{err.Error()}
		}

		if parsedURL.Scheme != "https" {
			return nil, FormatError{fmt.Sprintf("unexpected scheme %q in pre-signed URL", parsedURL.Scheme)}
		}

		if err = v.verifyHost(parsedURL.Host); err != nil {
			return nil, err
		}
		parsedURL.Host = v.stsEndpoint

		if parsedURL.Path != "/" {
			return nil, FormatError{"unexpected path in pre-signed URL"}
		}

		queryParamsLower := make(url.Values)
		queryParams := parsedURL.Query()
		for key, values := range queryParams {
			if !parameterWhitelist[strings.ToLower(key)] {
				return nil, FormatError{fmt.Sprintf("non-whitelisted query parameter %q", key)}
			}
			if len(values) != 1 {
				return nil, FormatError{"query parameter with multiple values not supported"}
			}
			queryParamsLower.Set(strings.ToLower(key), values[0])
		}

		if queryParamsLower.Get("action") != "GetCallerIdentity" {
			return nil, FormatError{"unexpected action parameter in pre-signed URL"}
		}

		if err = v.verifyClusterID(queryParamsLower.Get("clusterid")); err != nil {
			return nil, err
		}
		accessKeyId = queryParamsLower.Get("accesskeyid")

		req, err = http.NewRequest("GET", parsedURL.String(), nil)
		req.Header.Set("User-Agent", userAgentV1)
	}

	req.Header.Set("accept", "application/json")
	response, err := v.client.Do(req)
	if err != nil {
		// special case to avoid printing the full URL if possible
		if urlErr, ok := err.(*url.Error); ok {
			log.WithError(urlErr.Err).Errorf("error during GET")
			return nil, newOpenAPIErr(http.StatusBadRequest, nil, urlErr.Err)
		}
		log.WithError(err).Errorf("error during GET")
		return nil, newOpenAPIErr(http.StatusBadRequest, nil, err)
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		responseBytes, err := ioutil.ReadAll(response.Body)
		if err != nil {
			log.Errorf("error from RAM (expected 200, got %d, err %v)", response.StatusCode, err)
			return nil, newOpenAPIErr(response.StatusCode, nil, nil)
		}
		log.Errorf("error from RAM (expected 200, got %d, body %s, err %v)", response.StatusCode, string(responseBytes), err)
		return nil, newOpenAPIErr(response.StatusCode, responseBytes, nil)
	}

	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Errorf(fmt.Sprintf("error reading HTTP result: %v", err))
		return nil, newOpenAPIErr(http.StatusBadRequest, nil, fmt.Errorf("error reading HTTP result: %s", err.Error()))
	}

	var callerIdentity getCallerIdentityWrapper
	err = json.Unmarshal(responseBody, &callerIdentity)
	if err != nil {
		log.Errorf(err.Error())
		return nil, newOpenAPIErr(http.StatusBadRequest, nil, err)
	}

	// parse the response into an Identity
	id := &Identity{
		ARN:       callerIdentity.Arn,
		AccountID: callerIdentity.AccountID,
	}
	id.CanonicalARN, err = arn.Canonicalize(id.ARN)
	if err != nil {
		log.Errorf(err.Error())
		return nil, newOpenAPIErr(http.StatusBadRequest, nil, err)
	}
	id.AccessKeyID = accessKeyId

	// The user ID is either UserID:SessionName (for assumed roles) or just
	// UserID (for RAM User principals).
	userIDParts := strings.Split(callerIdentity.PrincipalID, ":")
	if len(userIDParts) == 2 {
		id.UserID = userIDParts[0]
		id.SessionName = userIDParts[1]
	} else if len(userIDParts) == 1 {
		id.UserID = userIDParts[0]
	} else {
		return nil, newOpenAPIErr(http.StatusBadRequest, nil, fmt.Errorf(
			"malformed UserID %q",
			callerIdentity.PrincipalID))
	}

	return id, nil
}

// NewJSONStruct new a json struct
func NewJSONStruct() *JSONStruct {
	return &JSONStruct{}
}

// Load file
func (jst *JSONStruct) Load(filename string, v interface{}) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return
	}
	err = json.Unmarshal(data, &v)
	if err != nil {
		return
	}
}

// Get credentials from HomeDir
func getCredentialsFile() string {
	usr, err := user.Current()
	if err != nil {
		fmt.Println("Error:", err)
	}
	return usr.HomeDir + "/.acs/credentials"
}

// GetWithSTS returns a token valid for clusterID using the given STS client.
func (g generator) GetWithSTS(clusterID string, stsClient *sts.Client) (Token, error) {
	// generate an sts:GetCallerIdentity request and add our custom cluster ID header
	accessKey, err := stsClient.GetAccessKeyId()
	if err != nil {
		return Token{}, err
	}
	accessSecret, err := stsClient.GetAccessKeySecret()
	if err != nil {
		return Token{}, err
	}
	securityToken, err := stsClient.GetSecurityToken()
	if err != nil {
		return Token{}, err
	}

	queryStr := "SignatureVersion=" + stsSignVersion
	queryStr += "&Format=" + respBodyFormat
	queryStr += "&Timestamp=" + url.QueryEscape(time.Now().UTC().Format(timeFormat))
	queryStr += "&AccessKeyId=" + tea.StringValue(accessKey)
	queryStr += "&SignatureMethod=HMAC-SHA1"
	queryStr += "&Version=" + stsAPIVersion
	queryStr += "&SignatureNonce=" + uuid.NewV4().String()
	queryStr += "&Action=GetCallerIdentity"
	queryStr += "&ClusterId=" + clusterID
	if tea.StringValue(securityToken) != "" {
		queryStr += "&SecurityToken=" + url.QueryEscape(tea.StringValue(securityToken))
	}
	queryParams, err := url.ParseQuery(queryStr)
	if err != nil {
		return Token{}, err
	}
	result := queryParams.Encode()

	strToSign := httpGet + "&" + percentEncode + "&" + url.QueryEscape(result)
	hashSign := hmac.New(sha1.New, []byte(tea.StringValue(accessSecret)+"&"))
	hashSign.Write([]byte(strToSign))
	signature := base64.StdEncoding.EncodeToString(hashSign.Sum(nil))

	// Build url
	getCallerIdentityURL := stsHost + "?" + queryStr + "&Signature=" + url.QueryEscape(signature)

	// Set token expiration to 1 minute before the presigned URL expires for some cushion
	tokenExpiration := time.Now().Local().Add(presignedURLExpiration - 1*time.Minute)
	// TODO: this may need to be a constant-time base64 encoding
	return Token{v1Prefix + base64.StdEncoding.EncodeToString([]byte(getCallerIdentityURL)), tokenExpiration}, nil

}
