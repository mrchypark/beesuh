# Compos trusted-producer contract

`POST /v1/compos` accepts requests only through
`httpapi.NewAuthenticatedWithComposProducer`. The host authentication callback
establishes the `Principal`. The producer callback must grant the principal
permission to originate Compos executions in that workspace.

`httpapi.NewAuthenticated` does not grant this permission. It rejects Compos
requests. Operator bearer authentication also does not grant this permission.

The request `_compos.workspace_id` must equal the authenticated principal
workspace. The runtime scopes accepted `execution_request_id` values to that
principal and workspace. `subject_ref` does not establish a principal, grant a
permission, or select a workspace.

The accepted `execution_request_id` and `subject_ref` values are producer
assertions. They do not prove that Conductor started an execution. An authorized
producer can assert false provenance within its permitted scope.

A downstream decision that needs verified Conductor provenance must use a
signed Conductor attestation, or a lookup that binds the execution ID,
workspace, subject, and relevant payload. An existence-only lookup does not
prove those bindings.
