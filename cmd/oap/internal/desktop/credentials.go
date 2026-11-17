package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ValidateAccountModel checks the server's account-specific catalog; merely
// posting an arbitrary registration ID from the webview never selects an account.
func (c Config) ValidateAccountModel(ctx context.Context, support string) error {
	if !AccountAuthenticated(c.Model.Provider) {
		return nil
	}
	models, err := ChatGPTClient(support).Models(ctx, c.Model.Account)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.Slug == c.Model.Name {
			return nil
		}
	}
	return fmt.Errorf("selected model is not available to this ChatGPT account")
}

// SyncAccountTokens publishes only access tokens. Refresh tokens remain on the
// desktop host. Existing sessions retain their original account's Secret ref
// when the default account changes, and continue receiving its token updates.
func SyncAccountTokens(ctx context.Context, support string, c client.Client) error {
	auth := ChatGPTClient(support)
	accounts, err := auth.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	var sessions v1.AgentSessionList
	if err = c.List(ctx, &sessions); err != nil {
		return fmt.Errorf("list sessions for model credential rotation: %w", err)
	}
	var failures []error
	for _, a := range accounts {
		token := ""
		if a.RefreshToken != "" {
			fresh, e := auth.Access(ctx, a.ClientID)
			if e != nil {
				failures = append(failures, e)
				if a.ExpiresAt.After(time.Now()) {
					token = a.AccessToken
				}
			} else {
				token = fresh.AccessToken
			}
		}
		central := client.ObjectKey{Namespace: "agentprimitives-system", Name: AccountTokenSecretName(a.ClientID)}
		// Do not create unused account Secrets. Configure/reapply owns initial
		// creation and adoption labels; this loop only rotates existing values.
		var secret corev1.Secret
		if err = c.Get(ctx, central, &secret); client.IgnoreNotFound(err) != nil {
			return err
		} else if err != nil {
			continue
		}
		if err = replaceToken(ctx, c, central, "token", token); err != nil {
			return err
		}
		for _, sess := range sessions.Items {
			es := sess.Status.EffectiveSettings
			if es == nil || es.ModelTokenSource == nil {
				continue
			}
			ref := es.Model.APIKey
			if ref.Name == "" || ref.Key == "" {
				continue
			}
			var copy corev1.Secret
			if err = c.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: ref.Name}, &copy); client.IgnoreNotFound(err) != nil {
				return err
			} else if err != nil {
				continue
			}
			if copy.Annotations[v1.ModelTokenSourceAnnotation] != central.Namespace+"/"+central.Name+"/token" {
				continue
			}
			if err = replaceToken(ctx, c, client.ObjectKey{Namespace: sess.Namespace, Name: ref.Name}, ref.Key, token); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return errors.Join(failures...)
}

func replaceToken(ctx context.Context, c client.Client, key client.ObjectKey, field, token string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var secret corev1.Secret
		if err := c.Get(ctx, key, &secret); err != nil {
			return err
		}
		if string(secret.Data[field]) == token {
			return nil
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[field] = []byte(token)
		return c.Update(ctx, &secret)
	})
}
