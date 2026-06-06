package authz

import _ "embed"

// ModelText is the Casbin model definition (config/rbac_model.conf), embedded at
// build time so a deployed binary carries the model with no external file on
// disk. The composition root passes it to NewCasbinAuthorizer; tests may pass it
// too, or substitute a model of their own. See the .conf file for the model's
// semantics (flat roles, "*" wildcard, implicit-allow effect).
//
//go:embed config/rbac_model.conf
var ModelText string
