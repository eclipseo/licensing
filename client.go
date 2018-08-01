package licensing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/libtrust"
	"github.com/docker/licensing/lib/errors"
	"github.com/docker/licensing/lib/go-auth/jwt"
	"github.com/docker/licensing/lib/go-clientlib"
	"github.com/docker/licensing/model"
)

const (
	TRIAL_PRODUCT_ID   = "docker-ee-trial"
	TRIAL_RATE_PLAN_ID = "free-trial"
)

type Client interface {
	LoginViaAuth(ctx context.Context, username, password string) (authToken string, err error)
	GetHubUserOrgs(ctx context.Context, authToken string) (orgs []model.Org, err error)
	GetHubUserByName(ctx context.Context, username string) (user *model.User, err error)
	VerifyLicense(ctx context.Context, license model.IssuedLicense) (res *model.CheckResponse, err error)
	GenerateNewTrialSubscription(ctx context.Context, authToken, dockerID, email string) (subscriptionID string, err error)
	ListSubscriptions(ctx context.Context, authToken, dockerID string) (response []*model.SubscriptionDetail, err error)
	DownloadLicenseFromHub(ctx context.Context, authToken, subscriptionID string) (license *model.IssuedLicense, err error)
	ParseLicense(license []byte) (parsedLicense *model.IssuedLicense, err error)
}

func (c *client) LoginViaAuth(ctx context.Context, username, password string) (authToken string, err error) {
	creds, err := c.login(ctx, username, password)
	if err != nil {
		return "", errors.Wrap(err, errors.Fields{
			"username": username,
		})
	}

	return creds.Token, nil
}

func (c *client) GetHubUserOrgs(ctx context.Context, authToken string) (orgs []model.Org, err error) {
	ctx = jwt.NewContext(ctx, authToken)

	orgs, err = c.getUserOrgs(ctx, model.PaginationParams{})
	if err != nil {
		return nil, errors.WithMessage(err, "Failed to get orgs for user")
	}

	return orgs, nil
}

func (c *client) GetHubUserByName(ctx context.Context, username string) (user *model.User, err error) {
	user, err = c.getUserByName(ctx, username)
	if err != nil {
		return nil, errors.Wrap(err, errors.Fields{
			"username": username,
		})
	}

	return user, nil
}

func (c *client) VerifyLicense(ctx context.Context, license model.IssuedLicense) (res *model.CheckResponse, err error) {
	res, err = c.check(ctx, license)
	if err != nil {
		return nil, errors.WithMessage(err, "Failed to verify license")
	}

	return res, nil
}

func (c *client) GenerateNewTrialSubscription(ctx context.Context, authToken, dockerID, email string) (subscriptionID string, err error) {
	ctx = jwt.NewContext(ctx, authToken)

	_, err = c.getAccount(ctx, dockerID)
	if err != nil {
		code, ok := errors.HTTPStatus(err)
		// create billing account if one is not found
		if ok && code == http.StatusNotFound {
			_, err = c.createAccount(ctx, dockerID, &model.AccountCreationRequest{
				Profile: model.Profile{
					Email: email,
				},
			})
			if err != nil {
				return "", errors.Wrap(err, errors.Fields{
					"dockerID": dockerID,
					"email":    email,
				})
			}
		} else {
			return "", errors.Wrap(err, errors.Fields{
				"dockerID": dockerID,
			})
		}
	}

	sub, err := c.createSubscription(ctx, &model.SubscriptionCreationRequest{
		Name:            "Docker Enterprise Free Trial",
		DockerID:        dockerID,
		ProductID:       TRIAL_PRODUCT_ID,
		ProductRatePlan: TRIAL_RATE_PLAN_ID,
		Eusa: &model.EusaState{
			Accepted: true,
		},
	})
	if err != nil {
		return "", errors.Wrap(err, errors.Fields{
			"dockerID": dockerID,
			"email":    email,
		})
	}

	return sub.ID, nil
}

func (c *client) ListSubscriptions(ctx context.Context, authToken, dockerID string) (response []*model.SubscriptionDetail, err error) {
	ctx = jwt.NewContext(ctx, authToken)

	subs, err := c.listSubscriptions(ctx, map[string]string{"docker_id": dockerID})
	if err != nil {
		return nil, errors.Wrap(err, errors.Fields{
			"dockerID": dockerID,
		})
	}

	// filter out non docker licenses
	dockerSubs := []*model.SubscriptionDetail{}
	for _, sub := range subs {
		if !strings.HasPrefix(sub.ProductID, "docker-ee") {
			continue
		}

		dockerSubs = append(dockerSubs, sub)
	}

	return dockerSubs, nil
}

func (c *client) DownloadLicenseFromHub(ctx context.Context, authToken, subscriptionID string) (license *model.IssuedLicense, err error) {
	ctx = jwt.NewContext(ctx, authToken)

	license, err = c.getLicenseFile(ctx, subscriptionID)
	if err != nil {
		return nil, errors.Wrap(err, errors.Fields{
			"subscriptionID": subscriptionID,
		})
	}

	return license, nil
}

func (c *client) ParseLicense(license []byte) (parsedLicense *model.IssuedLicense, err error) {
	parsedLicense = &model.IssuedLicense{}
	err = json.Unmarshal(license, &parsedLicense)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to parse license")
	}

	return parsedLicense, nil
}

type client struct {
	publicKey libtrust.PublicKey
	hclient   *http.Client
	baseURI   url.URL
}

// Config holds licensing client configuration
type Config struct {
	BaseURI    url.URL
	HTTPClient *http.Client
	// used by licensing client to validate an issued license
	PublicKey string
}

func errorSummary(body []byte) string {
	var be struct {
		Message string `json:"message"`
	}

	jsonErr := json.Unmarshal(body, &be)
	if jsonErr != nil {
		return clientlib.DefaultErrorSummary(body)
	}

	return be.Message
}

// New creates a new licensing Client
func New(config *Config) (Client, error) {
	publicKey, err := unmarshalPublicKey(config.PublicKey)
	if err != nil {
		return nil, err
	}

	hclient := config.HTTPClient
	if hclient == nil {
		hclient = &http.Client{}
	}

	return &client{
		baseURI:   config.BaseURI,
		hclient:   hclient,
		publicKey: publicKey,
	}, nil
}

func unmarshalPublicKey(publicKey string) (libtrust.PublicKey, error) {
	pemBytes, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil {
		return nil, errors.Wrapf(err, errors.Fields{
			"public_key": publicKey,
		}, "decode public key failed")
	}

	key, err := libtrust.UnmarshalPublicKeyPEM(pemBytes)
	if err != nil {
		return nil, errors.Wrapf(err, errors.Fields{
			"public_key": publicKey,
		}, "unmarshal public key failed")
	}
	return key, nil
}

func (c *client) doReq(ctx context.Context, method string, url *url.URL, opts ...clientlib.RequestOption) (*http.Request, *http.Response, error) {
	return clientlib.Do(ctx, method, url.String(), append(c.requestDefaults(), opts...)...)
}

func (c *client) doRequestNoAuth(ctx context.Context, method string, url *url.URL, opts ...clientlib.RequestOption) (*http.Request, *http.Response, error) {
	return clientlib.Do(ctx, method, url.String(), append(c.requestDefaults(), opts...)...)
}

func (c *client) requestDefaults() []clientlib.RequestOption {
	return []clientlib.RequestOption{
		func(req *clientlib.Request) {
			tok, _ := jwt.FromContext(req.Context())
			req.Header.Add("Authorization", "Bearer "+tok)
			req.ErrorSummary = errorSummary
			req.Client = c.hclient
		},
	}
}