# OAuth access-token delivery

`goauthy.NewOAuthClient`는 API-key 어댑터와 별도로 GoAuthy의 11-field OAuth 응답을
검증하고 고정 HTTPS 모델 POST에 `Authorization: Bearer`를 주입한다. 기존 API-key
버전 pin과 DTO는 변경하지 않는다. OAuth 사용은 host가 명시적으로 선택한다.

```go
client, err := goauthy.NewOAuthClient(issuer, binding, source,
    issuerHTTPClient, providerHTTPClient, nil) // nil clock = time.Now
// 오류를 처리한 뒤, 소유 Connection에 연결한 client를 재사용한다.
return beesuh.Credential{HTTPClient: client, AccountID: binding.AccountID}, nil
```

`OAuthBinding`의 grant/provider/opaque connection generation/account/scopes/resource/
consumer/endpoint/초기 CredentialVersion은 승인 후 인증된 서버 검토 결과에서 가져온다.
최초 delivery 응답으로 기대값을 채우지 않는다. Scopes는 허용 목록이며 응답은 중복 없는
부분집합이어야 한다. 필수 scope가 있는 업무의 실행 가능 여부는 host가 별도로 확인한다.
OAuth handoff의 review_digest를 빈 grant.connector_digest와 비교하지 않는다.

source는 `OAuthTokenRequest{Resource, ConsumerClientID}`를 받아 해당 consumer 자신의
현재 human use token을 공급한다. 소유권과 서비스 세션 연결, issuer/client/resource/scope/
subject/expiry 검증 및 취득은 trusted host source 책임이다. DTO에 owner/resource/consumer는
없으므로 어댑터가 응답만으로 이들을 검증했다고 주장하지 않는다. GoAuthy는 현재 grant와
토큰의 권한을 검사한다. 다른 RP 토큰이나 Beesuh API용 resource 토큰을 대체하지 않는다.

각 모델 호출마다 credential endpoint를 새로 호출한다. kind=oauth2, token_type=Bearer,
grant/provider/generation/account, scopes, 양의 int64 version, 두 expiry를 검증한다.
누락/null/중복/추가 필드는 거절하므로 refresh token/client secret을 받지 않는다.
토큰과 consent 모두 현재 시각보다 미래에 만료되어야 하며 dispatch 직전 다시 확인한다.
실패 시 다른 계정/캐시 토큰으로 fallback하지 않는다. 자동 refresh, 재시도, redirect는 없다.

승인 후 동일 generation/계정/허용 scope의 version 증가는 수용하고 관측 하한보다 낮은
버전은 거절한다. 같은 버전의 재사용은 허용한다. 클라이언트마다 credential 조회부터 모델
dispatch까지 직렬화해 같은 인스턴스의 조회 경쟁을 막는다. `ResolveCredential`에서 매번
새 클라이언트를 만들지 않는다. 관측 하한은 메모리에만 있으며 재시작 또는 여러 인스턴스
사이의 보존·공유는 host 책임이다. 새 인스턴스의 `CredentialVersion`에는 host가 보존한
하한을 제공해야 한다. 글로벌 CAS나 토큰 저장소는 추가하지 않았다.

`Credential.HTTPClient`의 기존 runtime 경로를 사용한다. Connection 소유권·대상 provider를
검사한 뒤 host resolver를 호출하며, 다른 Connection은 별도 client를 사용한다.
프로파일을 재연결하거나 기대 account/generation이 바뀌면 기존 binding/client를 재사용하지
않는다. client/source/반환 DTO의 비밀을 로그나 Rhiza journal에 직렬화하지 않는다.

검증 명령:

```sh
go test -race ./goauthy . -run 'Test(OAuth|GoAuthyOAuth)' -count=1
```

고정 clock과 합성 외부 HTTPS issuer/model을 사용한다. Runtime/Rhiza/인증 transport는 실제
구현이다. 두 프로파일의 v1→v2 실행, rollback 및 grant 철회 후 모델 전송 차단, 다른
프로파일 정상 실행, journal 비밀 비노출을 검사한다. 공개 BFF callback/state 소비,
실제 GoAuthy OAuth→Beesuh live bridge, Codex account/token 호환, 자동 refresh는 이 검증에
포함하지 않는다. owner refresh endpoint를 consumer Bearer로 호출하지 않는다.
