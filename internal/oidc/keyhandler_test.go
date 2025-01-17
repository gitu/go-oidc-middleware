package oidc

import (
	"context"
	"encoding/json"
	"github.com/lestrrat-go/jwx/jwa"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/jwk"
	"github.com/stretchr/testify/require"
	"github.com/xenitab/go-oidc-middleware/optest"
)

func TestNewKeyHandler(t *testing.T) {
	ctx := context.Background()

	op := optest.NewTesting(t)
	issuer := op.GetURL(t)
	discoveryUri := GetDiscoveryUriFromIssuer(issuer)
	jwksUri, err := getJwksUriFromDiscoveryUri(http.DefaultClient, discoveryUri, 10*time.Millisecond)
	require.NoError(t, err)

	keyHandler, err := newKeyHandler(http.DefaultClient, jwksUri, 10*time.Millisecond, 100, false)
	require.NoError(t, err)

	keySet1 := keyHandler.getKeySet()
	require.Equal(t, 1, keySet1.Len())

	expectedKey1, ok := keySet1.Get(0)
	require.True(t, ok)

	token1 := op.GetToken(t)

	headers1, err := getHeadersFromTokenString(token1.AccessToken)
	require.NoError(t, err)

	keyID1, err := getKeyIDFromTokenHeader(headers1)
	require.NoError(t, err)

	tokenAlgorithm, err := getTokenAlgorithmFromTokenHeader(headers1)
	require.NoError(t, err)

	// Test valid key id
	key1, err := keyHandler.getKeyFromID(ctx, keyID1, tokenAlgorithm)
	require.NoError(t, err)
	require.Equal(t, expectedKey1, key1)

	// Test invalid key id
	_, err = keyHandler.getKeyFromID(ctx, "foo", tokenAlgorithm)
	require.Error(t, err)

	// Test with rotated keys
	op.RotateKeys(t)

	token2 := op.GetToken(t)

	headers2, err := getHeadersFromTokenString(token2.AccessToken)
	require.NoError(t, err)

	keyID2, err := getKeyIDFromTokenHeader(headers2)
	require.NoError(t, err)

	tokenAlgorithm2, err := getTokenAlgorithmFromTokenHeader(headers2)
	require.NoError(t, err)

	key2, err := keyHandler.getKeyFromID(ctx, keyID2, tokenAlgorithm2)
	require.NoError(t, err)

	keySet2 := keyHandler.getKeySet()
	require.Equal(t, 1, keySet2.Len())

	expectedKey2, ok := keySet2.Get(0)
	require.True(t, ok)

	require.Equal(t, expectedKey2, key2)

	// Test that old key doesn't match new key
	require.NotEqual(t, key1, key2)

	// Validate that error is returned when using fake jwks uri
	_, err = newKeyHandler(http.DefaultClient, "http://foo.bar/baz", 10*time.Millisecond, 100, false)
	require.Error(t, err)

	// Validate that error is returned when keys are rotated,
	// new token with new key and jwks uri isn't accessible
	op.RotateKeys(t)
	token3 := op.GetToken(t)

	headers3, err := getHeadersFromTokenString(token3.AccessToken)
	require.NoError(t, err)

	keyID3, err := getKeyIDFromTokenHeader(headers3)
	require.NoError(t, err)
	op.Close(t)
	_, err = keyHandler.getKeyFromID(ctx, keyID3, "")
	require.Error(t, err)
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()

	op := optest.NewTesting(t)
	issuer := op.GetURL(t)
	discoveryUri := GetDiscoveryUriFromIssuer(issuer)
	jwksUri, err := getJwksUriFromDiscoveryUri(http.DefaultClient, discoveryUri, 10*time.Millisecond)
	require.NoError(t, err)

	rateLimit := uint(10)
	keyHandler, err := newKeyHandler(http.DefaultClient, jwksUri, 10*time.Millisecond, rateLimit, false)
	require.NoError(t, err)

	require.Equal(t, 1, keyHandler.keyUpdateCount)

	_, err = keyHandler.waitForUpdateKeySetAndGetKeySet(ctx)
	require.NoError(t, err)

	require.Equal(t, 2, keyHandler.keyUpdateCount)

	concurrentUpdate := func(workers int) {
		wg1 := sync.WaitGroup{}
		wg1.Add(1)

		wg2 := sync.WaitGroup{}
		for i := 0; i < workers; i++ {
			wg2.Add(1)
			go func() {
				wg1.Wait()
				_, err := keyHandler.waitForUpdateKeySetAndGetKeySet(ctx)
				require.NoError(t, err)
				wg2.Done()
			}()
		}
		wg1.Done()
		wg2.Wait()
	}

	concurrentUpdate(100)
	require.Equal(t, 3, keyHandler.keyUpdateCount)
	concurrentUpdate(100)
	require.Equal(t, 4, keyHandler.keyUpdateCount)
	concurrentUpdate(100)
	require.Equal(t, 5, keyHandler.keyUpdateCount)

	multipleConcurrentUpdates := func() {
		wg1 := sync.WaitGroup{}
		wg1.Add(1)

		wg2 := sync.WaitGroup{}
		for i := 0; i < 10; i++ {
			wg2.Add(1)
			go func() {
				wg1.Wait()
				concurrentUpdate(10)
				wg2.Done()
			}()
		}
		wg1.Done()
		wg2.Wait()
	}

	multipleConcurrentUpdates()
	require.Equal(t, 6, keyHandler.keyUpdateCount)

	// test rate limit
	time.Sleep(10 * time.Millisecond)
	start := time.Now()
	_, err = keyHandler.waitForUpdateKeySetAndGetKeySet(ctx)
	require.NoError(t, err)
	stop := time.Now()
	expectedStop := start.Add(time.Second / time.Duration(rateLimit))

	require.WithinDuration(t, expectedStop, stop, 20*time.Millisecond)

	require.Equal(t, 7, keyHandler.keyUpdateCount)
}

func TestNewKeyHandlerWithKeyIDDisabled(t *testing.T) {
	disableKeyID := true
	keySets := testNewTestKeySet(t)

	keySets.setKeys(testNewKeySet(t, 1, disableKeyID))

	testServer := testNewJwksServer(t, keySets)
	defer testServer.Close()

	_, err := newKeyHandler(http.DefaultClient, testServer.URL, 10*time.Millisecond, 100, disableKeyID)
	require.NoError(t, err)

	keySets.setKeys(testNewKeySet(t, 2, disableKeyID))

	_, err = newKeyHandler(http.DefaultClient, testServer.URL, 10*time.Millisecond, 100, disableKeyID)
	require.Error(t, err)
}

func TestNewKeyHandlerWithKeyIDEnabled(t *testing.T) {
	disableKeyID := false
	keySets := testNewTestKeySet(t)

	keySets.setKeys(testNewKeySet(t, 1, disableKeyID))

	testServer := testNewJwksServer(t, keySets)
	defer testServer.Close()

	_, err := newKeyHandler(http.DefaultClient, testServer.URL, 10*time.Millisecond, 100, disableKeyID)
	require.NoError(t, err)

	keySets.setKeys(testNewKeySet(t, 2, disableKeyID))

	_, err = newKeyHandler(http.DefaultClient, testServer.URL, 10*time.Millisecond, 100, disableKeyID)
	require.NoError(t, err)
}

func TestUpdateKeySetWithKeyIDDisabled(t *testing.T) {
	ctx := context.Background()

	disableKeyID := true
	keySets := testNewTestKeySet(t)

	keySets.setKeys(testNewKeySet(t, 1, disableKeyID))

	testServer := testNewJwksServer(t, keySets)
	defer testServer.Close()

	keyHandler, err := newKeyHandler(http.DefaultClient, testServer.URL, 10*time.Millisecond, 100, disableKeyID)
	require.NoError(t, err)

	_, err = keyHandler.updateKeySet(ctx)
	require.NoError(t, err)

	keySets.setKeys(testNewKeySet(t, 2, disableKeyID))

	_, err = keyHandler.updateKeySet(ctx)
	require.Error(t, err)
}

func TestUpdateKeySetWithKeyIDEnabled(t *testing.T) {
	ctx := context.Background()

	disableKeyID := false
	keySets := testNewTestKeySet(t)

	keySets.setKeys(testNewKeySet(t, 1, disableKeyID))

	testServer := testNewJwksServer(t, keySets)
	defer testServer.Close()

	keyHandler, err := newKeyHandler(http.DefaultClient, testServer.URL, 100*time.Millisecond, 100, disableKeyID)
	require.NoError(t, err)

	_, err = keyHandler.updateKeySet(ctx)
	require.NoError(t, err)

	keySets.setKeys(testNewKeySet(t, 2, disableKeyID))

	_, err = keyHandler.updateKeySet(ctx)
	require.NoError(t, err)
}

func TestWaitForUpdateKeySetWithKeyIDDisabled(t *testing.T) {
	ctx := context.Background()

	disableKeyID := true
	keySets := testNewTestKeySet(t)

	keySets.setKeys(testNewKeySet(t, 1, disableKeyID))

	testServer := testNewJwksServer(t, keySets)
	defer testServer.Close()

	keyHandler, err := newKeyHandler(http.DefaultClient, testServer.URL, 10*time.Millisecond, 100, disableKeyID)
	require.NoError(t, err)

	_, err = keyHandler.waitForUpdateKeySetAndGetKey(ctx)
	require.NoError(t, err)

	keySets.setKeys(testNewKeySet(t, 2, disableKeyID))

	_, err = keyHandler.waitForUpdateKeySetAndGetKey(ctx)
	require.Error(t, err)
}

func TestWaitForUpdateKeySetWithKeyIDEnabled(t *testing.T) {
	ctx := context.Background()

	disableKeyID := false
	keySets := testNewTestKeySet(t)

	keySets.setKeys(testNewKeySet(t, 1, disableKeyID))

	testServer := testNewJwksServer(t, keySets)
	defer testServer.Close()

	keyHandler, err := newKeyHandler(http.DefaultClient, testServer.URL, 10*time.Millisecond, 100, disableKeyID)
	require.NoError(t, err)

	_, err = keyHandler.waitForUpdateKeySetAndGetKey(ctx)
	require.NoError(t, err)

	keySets.setKeys(testNewKeySet(t, 2, disableKeyID))

	_, err = keyHandler.waitForUpdateKeySetAndGetKey(ctx)
	require.NoError(t, err)
}

func TestKeySetWithDuplicateKeyID(t *testing.T) {
	ctx := context.Background()

	keySets := testNewTestKeySet(t)
	keySets.setKeys(testNewDuplicateKeySet(t))

	testServer := testNewJwksServer(t, keySets)
	defer testServer.Close()

	keyHandler, err := newKeyHandler(http.DefaultClient, testServer.URL, 100*time.Millisecond, 100, false)
	require.NoError(t, err)

	genKey, _ := keySets.publicKeySet.Get(0)

	key256, err := keyHandler.getKeyFromID(ctx, genKey.KeyID(), jwa.RS256)
	require.NoError(t, err)
	require.Equal(t, "RS256", key256.Algorithm())

	key512, err := keyHandler.getKeyFromID(ctx, genKey.KeyID(), jwa.RS512)
	require.NoError(t, err)
	require.Equal(t, "RS512", key512.Algorithm())

	_, err = keyHandler.getKeyFromID(ctx, genKey.KeyID(), jwa.RS384)
	require.ErrorContains(t, err, "unable to find key")
}

func testNewJwksServer(t *testing.T, keySets *testKeySets) *httptest.Server {
	t.Helper()

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(keySets.publicKeySet)
		require.NoError(t, err)
	}))

	return testServer
}

type testKeySets struct {
	privateKeySet jwk.Set
	publicKeySet  jwk.Set
}

func testNewTestKeySet(t *testing.T) *testKeySets {
	t.Helper()

	return &testKeySets{}
}

func (k *testKeySets) setKeys(privKeySet jwk.Set, pubKeySet jwk.Set) {
	k.privateKeySet = privKeySet
	k.publicKeySet = pubKeySet
}

func testNewKeySet(t *testing.T, numKeys int, disableKeyID bool) (jwk.Set, jwk.Set) {
	t.Helper()

	privKeySet := jwk.NewSet()
	pubKeySet := jwk.NewSet()
	for i := 0; i < numKeys; i++ {
		privKey, pubKey := testNewKey(t)

		if disableKeyID {
			err := privKey.Remove(jwk.KeyIDKey)
			require.NoError(t, err)

			err = pubKey.Remove(jwk.KeyIDKey)
			require.NoError(t, err)
		}

		privKeySet.Add(privKey)
		pubKeySet.Add(pubKey)
	}

	return privKeySet, pubKeySet
}

func testNewDuplicateKeySet(t *testing.T) (jwk.Set, jwk.Set) {
	t.Helper()

	privKeySet := jwk.NewSet()
	pubKeySet := jwk.NewSet()

	privKey, pubKeyA, pubKeyB := testDuplicateKey(t)

	privKeySet.Add(privKey)
	pubKeySet.Add(pubKeyA)
	privKeySet.Add(privKey)
	pubKeySet.Add(pubKeyB)

	return privKeySet, pubKeySet
}
