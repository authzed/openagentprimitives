package dotenv

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WriteSecretKey creates-or-updates the Secret named secretName in namespace ns,
// setting the key secretKey to value. It returns created=true when the Secret
// did not previously exist.
func WriteSecretKey(
	ctx context.Context,
	c client.Client,
	ns, secretName, secretKey string,
	value []byte,
) (created bool, err error) {
	var sec corev1.Secret
	getErr := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: secretName}, &sec)
	switch {
	case errors.IsNotFound(getErr):
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{secretKey: value},
		}
		if err := c.Create(ctx, &sec); err != nil {
			return false, fmt.Errorf("create Secret %s/%s: %w", ns, secretName, err)
		}
		return true, nil
	case getErr != nil:
		return false, fmt.Errorf("get Secret %s/%s: %w", ns, secretName, getErr)
	default:
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[secretKey] = value
		if err := c.Update(ctx, &sec); err != nil {
			return false, fmt.Errorf("update Secret %s/%s: %w", ns, secretName, err)
		}
		return false, nil
	}
}
