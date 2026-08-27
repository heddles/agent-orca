# Deploy kind cluster for keycloak
kind create cluster --name keycloak --config ../kind-cluster.yaml

openssl req -subj '/CN=test.keycloak.org/O=Test Keycloak./C=US' -newkey rsa:2048 -nodes -keyout key.pem -x509 -days 365 -out certificate.pem
# Install keycloak operator and create TLS for local keycloak
kubectl create namespace keycloak
kubectl apply -k 'github.com/keycloak/keycloak-k8s-resources/kubernetes?ref=26.7.2'

kubectl create secret tls example-tls-secret --cert certificate.pem --key key.pem -n keycloak

# Deploy postgres db
kubectl apply -f postgres.yaml -n keycloak

# Deploy keycloak
kubectl create -n keycloak secret generic keycloak-db-secret \
  --from-literal=username=testuser \
  --from-literal=password=testpassword

# kubectl apply -n keycloak -f keycloak-client-secret.yaml

kubectl apply -f example-keycloak.yaml -n keycloak

# Install nginx ingress controller
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/main/deploy/static/provider/kind/deploy.yaml

# Create tenantconfig inside agentorca
kubectl apply -n agent-orca-system -f dev-tenant-keycloak.yaml
# kubectl apply -n agent-orca-system -f keycloak-client-secret.yaml

echo "Wait for the keycloak instance to finish standing up and add 127.0.0.1 test.keycloak.org to your /etc/hosts file"
