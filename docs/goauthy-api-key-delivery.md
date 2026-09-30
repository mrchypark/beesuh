# GoAuthy API-key 프로파일 실행

동일 사용자·workspace에 같은 provider를 가리키는 `personal`, `work` Connection을 등록하고
요청의 `connection_id`로 선택한다. 계정 연결·동의·비밀 저장·갱신은 GoAuthy와 서비스 BFF가
담당한다. Beesuh에는 별도 로그인 UI나 계정 registry를 추가하지 않는다.

```json
{"workspace_id":"workspace","session_key":"conversation","request_id":"request-1","connection_id":"personal","message":"안녕"}
```

서비스는 선택한 소유 Connection의 서버 전용 참조를 검증된 `APIKeyBinding`과 token source에
매핑한다. `goauthy.NewAPIKeyClient(issuer, binding, source, issuerClient, providerClient)` 결과를
`ResolveCredential`의 `Credential.HTTPClient`로 반환한다. 이 경로의 `AccountID`는 grant,
provider, opaque generation, credential version, digest를 포함한 binding의 안정적인 해시 등
논리 인증 식별자다. Codex app-server의 실제 ChatGPT account ID와 혼동하지 않는다.

Binding은 승인한 서버 설정이며 사용자 요청에서 만들지 않는다. endpoint는 서비스가 검토한
고정 HTTPS 모델 URL이다. GoAuthy 응답으로 URL을 결정하지 않는다. source는 해당 owner와
consumer에 허가된 현재 human token을 공급해야 한다. issuer가 resource/scope/consumer와
grant 소유권을 검사한다. GoAuthy 사용자 토큰을 모델에 전달하지 않는다.

모든 모델 HTTP 호출 직전에 빈 body로 GoAuthy credential endpoint를 호출한다. 열 개 필수
응답 필드와 binding을 비교하며 `connection_generation`은 opaque string,
`credential_version`은 양의 int64로 처리한다. 버전은 정확히 고정하므로 키 회전 후에는
서비스가 검토한 binding을 갱신해야 한다. 동의 만료, 불일치, 조회 실패 시 모델 전송을
중단하며 캐시 키·다른 계정·전역 로그인으로 fallback하지 않는다. 자동 재시도, redirect,
cookie jar를 사용하지 않는다. 키나 토큰은 Beesuh DB/journal에 저장하지 않는다.

원시 API key를 전달받은 뒤 발생한 upstream 효과는 GoAuthy revoke로 되돌릴 수 없다.
proxy operation 제한을 이 delivery가 강제하지도 않는다. 이미 유출된 키의 무효화는
provider의 키 폐기가 필요하다. OAuth/Codex credential delivery는 이 API-key 어댑터 범위 밖이다.

## 재현

```sh
go test -race ./goauthy . -run 'Test(APIKey|GoAuthyDeliveryProfiles)' -count=1
```

합성 HTTPS issuer와 실제 Runtime/Rhiza 및 합성 HTTPS 모델을 통해 같은 사용자의 두 계정 선택,
동시 실행, 매 사용 조회, 한 grant 거절 후 모델 전송 차단과 다른 계정 계속 실행,
journal 비밀 비노출을 검증한다. 패키지 테스트는 snapshot 타입·만료·metadata 불일치와
목적지 제한도 검증한다. 실제 GoAuthy endpoint 연결 완료를 뜻하지 않는다.

## 실제 issuer용 opt-in fixture

생산자가 disposable 실제 HTTPS GoAuthy issuer와 합성 HTTPS Chat Completions 모델을 운영한다.
외부 SaaS를 호출하지 않는다. 아래 JSON과 token 파일은 일반 파일, 권한 0600이어야 한다.
JSON의 정수는 문자열이나 float로 변환하지 않는다. CA 파일은 PEM이며 TLS 검증을 끄지 않는다.
두 profile의 endpoint는 동일한 loopback HTTPS URL이어야 한다.

```json
{
  "issuer": "https://127.0.0.1:8443",
  "issuer_ca_file": "/private/fixture/issuer-ca.pem",
  "provider_ca_file": "/private/fixture/model-ca.pem",
  "profiles": [
    {
      "connection_id": "personal",
      "token_file": "/private/fixture/personal-token",
      "expected_text": "personal",
      "binding": {
        "grant_id": "approved-personal-grant",
        "provider_id": "fixture-provider",
        "connection_generation": "opaque-generation",
        "credential_version": 1,
        "connector_digest": "exact-reviewed-digest",
        "header": "Authorization",
        "prefix": "Bearer ",
        "endpoint": "https://127.0.0.1:9443/v1/chat/completions"
      }
    }
  ]
}
```

배열에 실제 두 번째 `work` profile의 별도 grant/token/snapshot을 추가한다. 각 token은 이
소비자 자신의 confidential client로 발급된 human `goauthy.connections.use` 토큰이어야 한다.
source는 호출마다 token 파일을 다시 읽는다. 모델은 선택된 키에 따라 `expected_text`를
Chat Completions message content로 반환한다. 각 profile을 두 번 실행하므로 생산자는
issuer delivery 및 모델 호출 카운터가 profile별 2회인지 별도로 확인한다.

```sh
BEESUH_E2E_GOAUTHY_DELIVERY=1 \
BEESUH_E2E_GOAUTHY_DELIVERY_FIXTURE_FILE=/private/fixture/beesuh-delivery.json \
go test . -run '^TestGoAuthyDeliveryLiveE2E$' -count=1 -v
```

철회 검증 시 해당 profile에 `"expected_error": true`를 추가한다. 두 요청 모두 오류이며
성공 상태가 아님을 확인한 뒤 다음 profile을 계속 실행한다. 이 경우 `expected_text`는
생략할 수 있다. 생산자는 실제 issuer의 거절 상태와 모델 호출 수를 함께 검사해야 한다.

2026-09-07 실제 GoAuthy HTTPS standalone issuer와 인증서 검증을 유지한 로컬 TLS 모델로
`TestGoAuthyDeliveryLiveE2E`가 실행되었다. 자체 confidential consumer의 code+PKCE human
token을 사용해 personal/work의 정확한 API key가 각각 2회 전달되었다. work grant를
철회한 뒤 issuer credential POST의 404와 work의 추가 모델 호출 0회를 생산자가 확인했다.
personal은 2회 더 성공하여 총 호출은 personal 4회, work 2회였다. issuer 재시작 전후의
실행이 모두 통과했다. 로그 `/tmp/goauthy-beesuh-delivery-live-20260907.log`에서 두 번의
profile 검증·철회 차단 및 post-restart PASS를 확인했다. 정확한 key 비교와 호출 카운터는
GoAuthy producer의 검사 결과이며 비밀값은 로그에 남기지 않았다.

이 검증은 테스트용 Principal/Authorize callback을 사용한다. 공개 Beesuh 서비스 로그인,
BFF callback의 one-use state, OAuth/Codex token delivery, 외부 SaaS 호출은 포함하지 않는다.
wrong-consumer/proxy-only 거절의 Beesuh end-to-end 검증도 이 결과에 포함하지 않는다.
fixture/token 파일은 임시 0600 파일이며 production key 생성·배포·게시 없이 수행했다.
