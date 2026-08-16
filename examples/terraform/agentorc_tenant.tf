# Terraform example: provision a tenant in agent-orc
#
# This snippet shows how a platform team can onboard a new customer tenant
# using the same Kubernetes resources the admin API (POST /admin/tenants)
# creates under the hood. It round-trips with the API: the TenantConfig and
# client-secret Secret produced here are identical to what the admin API
# would write, so you can switch between `terraform apply` and `aoctl admin
# tenants create` without changing downstream behavior.
#
# Usage:
#   terraform init
#   terraform apply -var="tenant_name=acme" -var="client_id=acme-client"
#
# Requires the Kubernetes Terraform provider and the random provider:
#   terraform init -upgrade

terraform {
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.26"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6"
    }
  }
}

variable "tenant_name" {
  description = "Unique tenant identifier (used as the TenantConfig name and to derive the client-secret Secret name)."
  type        = string
}

variable "client_id" {
  description = "OAuth2 client ID for the tenant."
  type        = string
}

variable "client_secret" {
  description = "Client secret value. If empty, a random one is generated."
  type        = string
  default     = ""
  sensitive   = true
}

variable "operator_namespace" {
  description = "Namespace where the agent-orc operator runs (where TenantConfig lives)."
  type        = string
  default     = "agent-orc-system"
}

variable "target_namespace" {
  description = "Kubernetes namespace where the tenant's agents live. Defaults to tenant_name."
  type        = string
  default     = ""
}

variable "allowed_agents" {
  description = "List of agent names this tenant may invoke. Empty = all agents in the target namespace."
  type        = list(string)
  default     = []
}

variable "requests_per_minute" {
  description = "Max task submissions per minute (0 = unlimited)."
  type        = number
  default     = 0
}

variable "concurrent_runs" {
  description = "Max simultaneously running AgentRuns (0 = unlimited)."
  type        = number
  default     = 0
}

variable "budget_per_day_usd" {
  description = "Daily spend cap in USD (empty = unlimited)."
  type        = string
  default     = ""
}

locals {
  # Convention: <tenant-name>-client-secret (matches admin_api.go createTenant).
  secret_name = "${var.tenant_name}-client-secret"
  target_ns   = var.target_namespace != "" ? var.target_namespace : var.tenant_name

  # Build the rateLimit block only when at least one limit is set.
  rate_limit = merge(
    var.requests_per_minute > 0 ? { requestsPerMinute = var.requests_per_minute } : {},
    var.concurrent_runs > 0 ? { concurrentRuns = var.concurrent_runs } : {},
  )

  # Build the spec, adding optional fields only when provided.
  tenant_spec = merge(
    {
      authMode        = "issued"
      targetNamespace = local.target_ns
      issued = {
        clientID = var.client_id
        clientSecretRef = {
          name = local.secret_name
          key  = "client-secret"
        }
      }
    },
    length(var.allowed_agents) > 0 ? { allowedAgents = var.allowed_agents } : {},
    length(local.rate_limit) > 0 ? { rateLimit = local.rate_limit } : {},
    var.budget_per_day_usd != "" ? { budgetPerDayUSD = var.budget_per_day_usd } : {},
  )
}

# Generate a random client secret if one was not provided.
resource "random_password" "client_secret" {
  count   = var.client_secret == "" ? 1 : 0
  length  = 43
  special = false
  upper   = true
  lower   = true
  numeric = true
}

# 1. Create the client-secret Secret in the operator namespace.
resource "kubernetes_secret" "client_secret" {
  metadata {
    name      = local.secret_name
    namespace = var.operator_namespace
  }
  data = {
    "client-secret" = var.client_secret != "" ? var.client_secret : random_password.client_secret[0].result
  }
  type = "Opaque"
}

# 2. Create the TenantConfig CR.
resource "kubernetes_manifest" "tenant_config" {
  manifest = {
    apiVersion = "agentorc.agentorc.io/v1alpha1"
    kind       = "TenantConfig"
    metadata = {
      name      = var.tenant_name
      namespace = var.operator_namespace
    }
    spec = local.tenant_spec
  }

  depends_on = [kubernetes_secret.client_secret]
}

output "tenant_name" {
  value = var.tenant_name
}

output "client_id" {
  value = var.client_id
}

output "client_secret" {
  value     = var.client_secret != "" ? var.client_secret : random_password.client_secret[0].result
  sensitive = true
}

output "target_namespace" {
  value = local.target_ns
}
