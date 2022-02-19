package kube

import (
	"testing"

	"github.com/vikramraodp/fissile/model"
	"github.com/vikramraodp/fissile/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRBACAccountPSPKube(t *testing.T) {
	t.Parallel()

	resources, err := NewRBACAccount("the-name",
		&model.Configuration{
			Authorization: model.ConfigurationAuthorization{
				Accounts: map[string]model.AuthAccount{
					"the-name": {
						Roles:        []string{"a-role"},
						ClusterRoles: []string{"privileged-cluster-role"},
						UsedBy: map[string]struct{}{
							// This must be used by multiple instance groups to be serialized
							"foo": struct{}{},
							"bar": struct{}{},
						},
					},
				},
			},
		}, ExportSettings{})

	require.NoError(t, err)

	account := findKind(resources, "ServiceAccount")
	if assert.NotNil(t, account, "service account not found") {
		actualAccount, err := RoundtripKube(account)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
			apiVersion: "v1"
			kind: "ServiceAccount"
			metadata:
				name: "the-name"
				labels:
					app.kubernetes.io/component: the-name
		`, actualAccount)
		}
	}

	roleBinding := findKind(resources, "RoleBinding")
	if assert.NotNil(t, roleBinding, "role binding not found") {
		actualRole, err := RoundtripKube(roleBinding)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
				apiVersion: "rbac.authorization.k8s.io/v1"
				kind: "RoleBinding"
				metadata:
					name: "the-name-a-role-binding"
					labels:
						app.kubernetes.io/component: the-name-a-role-binding
				subjects:
				-	kind: "ServiceAccount"
					name: "the-name"
				roleRef:
					kind: "Role"
					name: "a-role"
					apiGroup: "rbac.authorization.k8s.io"
			`, actualRole)
		}
	}

	role := findKind(resources, "Role")
	if assert.NotNil(t, role, "role not found") {
		actualRole, err := RoundtripKube(role)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
				apiVersion: rbac.authorization.k8s.io/v1
				kind: Role
				metadata:
					labels:
						app.kubernetes.io/component: a-role
					name: a-role
				rules: []
			`, actualRole)
		}
	}

	clusterRoleBinding := findKind(resources, "ClusterRoleBinding")
	if assert.NotNil(t, clusterRoleBinding, "cluster role binding not found") {
		actualBinding, err := RoundtripKube(clusterRoleBinding)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
			apiVersion: "rbac.authorization.k8s.io/v1"
			kind: "ClusterRoleBinding"
			metadata:
				name: "the-name-privileged-cluster-role-cluster-binding"
				labels:
					app.kubernetes.io/component: the-name-privileged-cluster-role-cluster-binding
			subjects:
			-	kind: "ServiceAccount"
				name: "the-name"
				namespace: "~"
			roleRef:
				kind: "ClusterRole"
				name: "privileged-cluster-role"
				apiGroup: "rbac.authorization.k8s.io"
		`, actualBinding)
		}
	}

	clusterRole := findKind(resources, "ClusterRole")
	if assert.NotNil(t, clusterRole, "cluster role not found") {
		actualClusterRole, err := RoundtripKube(clusterRole)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
				apiVersion: rbac.authorization.k8s.io/v1
				kind: ClusterRole
				metadata:
					labels:
						app.kubernetes.io/component: privileged-cluster-role
					name: privileged-cluster-role
				rules: []
			`, actualClusterRole)
		}
	}

}

func TestNewRBACAccountHelm(t *testing.T) {
	t.Parallel()

	resources, err := NewRBACAccount("the-name",
		&model.Configuration{
			Authorization: model.ConfigurationAuthorization{
				Accounts: map[string]model.AuthAccount{
					"the-name": model.AuthAccount{
						Roles:        []string{"a-role"},
						ClusterRoles: []string{"nonprivileged"},
						UsedBy: map[string]struct{}{
							// This must be used by multiple instance groups to be serialized
							"foo": struct{}{},
							"bar": struct{}{},
						},
					},
				},
				ClusterRoles: map[string]model.AuthRole{
					"nonprivileged": {
						{
							APIGroups:     []string{"policy"},
							Resources:     []string{"podsecuritypolicies"},
							ResourceNames: []string{"nonprivileged"},
							Verbs:         []string{"use"},
						},
						{
							APIGroups:     []string{"imaginary"},
							Resources:     []string{"other"},
							ResourceNames: []string{"unchanged"},
							Verbs:         []string{"yank"},
						},
					},
				},
			},
		}, ExportSettings{
			CreateHelmChart: true,
		})

	require.NoError(t, err)
	require.Len(t, resources, 5, "Should have account, role binding, and cluster role binding")

	account := findKind(resources, "ServiceAccount")
	roleBinding := findKind(resources, "RoleBinding")
	role := findKind(resources, "Role")
	clusterRoleBinding := findKind(resources, "ClusterRoleBinding")
	clusterRole := findKind(resources, "ClusterRole")

	t.Run("NoAuth", func(t *testing.T) {
		t.Parallel()
		config := map[string]interface{}{
			"Values.kube.auth": "",
		}
		actualAccount, err := RoundtripNode(account, config)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
			`, actualAccount)
		}

		actualRoleBinding, err := RoundtripNode(roleBinding, config)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
			`, actualRoleBinding)
		}

		actualRole, err := RoundtripNode(role, config)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
			`, actualRole)
		}

		actualClusterRoleBinding, err := RoundtripNode(clusterRoleBinding, config)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
			`, actualClusterRoleBinding)
		}

		actualClusterRole, err := RoundtripNode(clusterRole, config)
		if assert.NoError(t, err) {
			testhelpers.IsYAMLEqualString(assert.New(t), `---
			`, actualClusterRole)
		}
	})
}

func TestNewRBACRoleKube(t *testing.T) {
	t.Parallel()

	rbacRole, err := NewRBACRole("the-name",
		RBACRoleKindRole,
		[]model.AuthRule{
			{
				APIGroups: []string{"api-group-1"},
				Resources: []string{"resource-b"},
				Verbs:     []string{"verb-iii"},
			},
		},
		ExportSettings{})

	require.NoError(t, err)

	actual, err := RoundtripKube(rbacRole)
	require.NoError(t, err)
	testhelpers.IsYAMLEqualString(assert.New(t), `---
		apiVersion: "rbac.authorization.k8s.io/v1"
		kind: "Role"
		metadata:
			name: "the-name"
			labels:
				app.kubernetes.io/component: the-name
		rules:
		-	apiGroups:
			-	"api-group-1"
			resources:
			-	"resource-b"
			verbs:
			-	"verb-iii"
	`, actual)
}

func TestNewRBACRoleHelm(t *testing.T) {
	t.Parallel()

	rbacRole, err := NewRBACRole("the-name",
		RBACRoleKindRole,
		[]model.AuthRule{
			{
				APIGroups: []string{"api-group-1"},
				Resources: []string{"resource-b"},
				Verbs:     []string{"verb-iii"},
			},
		},
		ExportSettings{
			CreateHelmChart: true,
		})

	require.NoError(t, err)

	t.Run("NoAuth", func(t *testing.T) {
		t.Parallel()
		config := map[string]interface{}{
			"Values.kube.auth": "",
		}

		actual, err := RoundtripNode(rbacRole, config)
		require.NoError(t, err)

		testhelpers.IsYAMLEqualString(assert.New(t), `---
		`, actual)
	})

	t.Run("HasAuth", func(t *testing.T) {
		t.Parallel()
		config := map[string]interface{}{
			"Values.kube.auth": "rbac",
		}

		actual, err := RoundtripNode(rbacRole, config)
		require.NoError(t, err)

		testhelpers.IsYAMLEqualString(assert.New(t), `---
			apiVersion: "rbac.authorization.k8s.io/v1"
			kind: "Role"
			metadata:
				name: "the-name"
				labels:
					app.kubernetes.io/component: the-name
					app.kubernetes.io/instance: MyRelease
					app.kubernetes.io/managed-by: Tiller
					app.kubernetes.io/name: MyChart
					app.kubernetes.io/version: 1.22.333.4444
					helm.sh/chart: MyChart-42.1_foo
					skiff-role-name: "the-name"
			rules:
			-	apiGroups:
				-	"api-group-1"
				resources:
				-	"resource-b"
				verbs:
				-	"verb-iii"
		`, actual)
	})
}

/*
func TestNewRBACClusterRolePSPKube(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	resource, err := NewRBACClusterRolePSP("the-name",
		ExportSettings{})

	if !assert.NoError(err) {
		return
	}

	actualCR, err := RoundtripKube(resource)
	if !assert.NoError(err) {
		return
	}
	testhelpers.IsYAMLEqualString(assert, `---
		apiVersion: "rbac.authorization.k8s.io/v1"
		kind: "ClusterRole"
		metadata:
			name: "psp-role-the-name"
			labels:
				app.kubernetes.io/component: psp-role-the-name
		rules:
		-	apiGroups:
			-	"extensions"
			resourceNames:
			-	"the-name"
			resources:
			-	"podsecuritypolicies"
			verbs:
			-	"use"
	`, actualCR)
}

func TestNewRBACClusterRolePSPHelm(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	resource, err := NewRBACClusterRolePSP("the_name",
		ExportSettings{
			CreateHelmChart: true,
		})
	if !assert.NoError(err) {
		return
	}

	config := map[string]interface{}{
		"Values.kube.auth":         "rbac",
		"Values.kube.psp.the_name": "foo",
		"Release.Namespace":        "namespace",
	}
	actualCR, err := RoundtripNode(resource, config)
	if !assert.NoError(err) {
		return
	}
	testhelpers.IsYAMLEqualString(assert, `---
		apiVersion: "rbac.authorization.k8s.io/v1"
		kind: "ClusterRole"
		metadata:
			name: "namespace-psp-role-the_name"
			labels:
				app.kubernetes.io/component: namespace-psp-role-the_name
				app.kubernetes.io/instance: MyRelease
				app.kubernetes.io/managed-by: Tiller
				app.kubernetes.io/name: MyChart
				app.kubernetes.io/version: 1.22.333.4444
				helm.sh/chart: MyChart-42.1_foo
				skiff-role-name: "namespace-psp-role-the_name"
		rules:
		-	apiGroups:
			-	"extensions"
			resourceNames:
			-	"foo"
			resources:
			-	"podsecuritypolicies"
			verbs:
			-	"use"
	`, actualCR)
}
*/
