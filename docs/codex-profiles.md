# Codex 계정 프로파일 격리

GoAuthy에 로그인한 사용자는 같은 provider의 여러 외부 계정을 연결하고 선택한다.
계정 연결·동의·비밀 저장·갱신의 원본은 GoAuthy의 공통 credential-profile 확장이다.
Beesuh는 허가된 프로파일을 받아 실행하는 consumer이며 별도 계정 연결 UI나 로그인
registry를 만들지 않는다. API-key 전달은 [GoAuthy delivery 어댑터](goauthy-api-key-delivery.md)를
사용한다. 일반 HTTP OAuth는 [별도 어댑터](goauthy-oauth-delivery.md)를 사용하며,
실제 Codex 토큰·계정 호환과 연결은 아직 검증이 필요하다.

## 실행 시 선택

기존 `Connection`을 재사용한다. 같은 사용자·workspace에 `personal`, `work` 두 connection을
등록하고 둘 다 같은 `TargetID: "codex"`를 가리킬 수 있다. `CredentialRef`는 host가 허가된
GoAuthy profile에 연결하는 서버 전용 참조다. 사용자는 `/v1/chat`의 `connection_id`로 선택한다.
자신에게 속하지 않거나 폐기된 connection은 토큰을 가져오기 전에 거절한다.

```json
{"workspace_id":"workspace","session_key":"conversation","request_id":"request-1","connection_id":"personal","message":"안녕"}
```

`GET /v1/providers`와 `GET /v1/connections`는 기존 소유권 경계 안에서 선택 가능한 ID를 제공한다.
사용자 토큰 자체를 Codex 토큰으로 전달하지 않는다. `RequireIdentity` 모드에서 host가
검증한 Principal과 `ResolveCredential`이 반드시 필요하다.

Service의 `codex-cli` provider 설정은 template이다. 설정의 `CodexHome`은 사용하지
않으며, 각 실행에서 resolver가 반환한 절대 경로 `CodexHome`과 `AccountID`를 검증한다.
resolver 실패나 유효하지 않은 값은 subprocess 생성 전에 거절한다. template home으로
fallback하지 않는다. Host는 해당 home이 허가된 계정의 로그인 store인지 보장해야 한다.
Operator 모드에서는 각 CLI provider의 명시적 home이 필요하며, 정규화한 중복 home은 거절한다.

## 실험적 app-server 인증 어댑터

`model.Config{Kind: "codex-app-server", Model: "..."}`를 명시적으로 선택한다.
기존 `codex-cli` managed-home 경로와 HTTP `codex-api` 경로는 유지된다.
Codex CLI 0.153.4의 `generate-json-schema --experimental`에서 확인한
`account/login/start`의 `chatgptAuthTokens`와 `account/chatgptAuthTokens/refresh`를 사용한다.
설치된 schema는 이 인증 모드를 unstable/internal-use로 표시한다. 사용자의 명시적 선택으로
구현했으며 공급자의 안정적 지원·모든 버전 호환을 보장하지 않는다.
[공식 app-server 문서](https://learn.chatgpt.com/docs/app-server)와 설치 버전의 schema를 함께 확인한다.

Host는 소유권·consumer consent·목적·provider destination·논리 인증 세대가 고정된
`model.CodexAuthSource`를 `Credential.CodexAuth`에 주입한다. `Credential.AccountID`는
해당 프로파일의 실제 ChatGPT account/workspace ID다. GoAuthy의 사용자 subject와 별개다.

```go
// source는 이 connection에 허가된 profile만 사용하는 host adapter다.
// 비밀의 취득/저장/갱신 API를 Beesuh가 추측하거나 구현하지 않는다.
return beesuh.Credential{
    AccountID: upstreamAccountID,
    CodexAuth: source,
}, nil
```

Source 입력은 `CodexAuthRequest{Reason, PreviousAccountID}`다. 최초 조회의 Reason은
`initial`, Codex의 갱신 요청은 `unauthorized`다. 결과는
`CodexAuthTokens{AccessToken, AccountID, PlanType}`이며 refresh token은 Codex에 보내지 않는다.
다른 account ID가 돌아오거나 권한이 사라지면 거절한다. 토큰 공급자는 context 취소를
준수하고, 갱신된 비밀의 안전한 저장이 완료된 뒤 결과를 반환해야 한다.

각 completion은 별도의 subprocess와 임시 HOME/CODEX_HOME을 사용한다. 토큰은 stdio RPC에만
전달하며 argv·환경변수·Beesuh DB/journal·로그에 넣지 않는다. 전역 Codex 계정을 전환하거나
기존 authstore/cookie를 읽지 않는다. Codex 내장 도구는 끄고, tool 요청은 Beesuh가 검증해
실행하도록 기존 JSON model bridge를 사용한다. 취소/오류/완료 시 해당 subprocess를 종료한다.

Beesuh의 대화 journal은 유지되지만 외부 app-server thread는 completion마다 새로 만든다.
따라서 다른 프로파일로 바꾸면 허가된 대화 문맥은 전달될 수 있어도 이전 계정의 opaque
provider state나 process는 재사용하지 않는다. 프로세스 풀 최적화보다 프로파일 격리를 우선했다.

## 갱신·폐기 경계

Runtime의 connection lock이 해당 connection의 외부 호출과 로컬 revoke를 직렬화한다.
토큰 공급 전후에 현재 소유권·membership·connection generation을 다시 확인한다.
다른 프로파일은 별도로 실행할 수 있다. 공급자는 GoAuthy 쪽 consumer 사용허가와 profile
logical generation도 검사해야 한다. 정상 OAuth token refresh가 논리 인증 세대를 바꾸는 것은
아니며, refresh 저장 CAS/revoke tombstone은 authoritative token source의 책임이다.
Beesuh의 메모리 callback만으로 여러 서비스 사이의 CAS나 이미 발생한 효과 취소를 보장하지 않는다.

## 검증 범위

Fake app-server 프로세스로 선택한 두 profile의 동시 실행, 프로파일별 refresh, 다른 계정
응답 거절, 사용자 소유권과 폐기, 취소, RPC 오류의 비밀 비노출을 검증한다. 이 검증은 실제
ChatGPT 계정 로그인·토큰 갱신 또는 GoAuthy 확장과의 live 통합 증거가 아니다.

2026-09-07 검증: `go test -race ./...`, `go vet ./...`, CLI build 통과.
설치된 Codex 0.153.4를 임시 빈 HOME/CODEX_HOME과 실제 adapter 제한 설정으로 실행하여
`initialize` → `initialized` → `account/read`를 확인했다. 인증된 계정이 없음을 확인한 뒤
프로세스를 종료했다. 이는 CLI/RPC 설정 호환 확인이며 로그인·모델 실행 검증이 아니다.
배포 host는 검증한 Codex binary를 `Command`로 고정해야 한다. 다른 버전에서 계약이
거절되면 자동으로 전역 인증이나 다른 계정으로 fallback하지 않는다.

2026-10-04 검증: 이 host의 Codex 0.152.0에서 생성한 experimental schema의
`turn/completed` 오류 variant를 확인했다. 구조화된 `unauthorized`는 Runtime에서 해당
connection만 `needs_login`으로 전환하며, 실패 결과를 저장하고 자동으로 재실행하지 않는다.
이 검증은 fixture 기반이다. 인증된 실제 provider 검증은 [배포 검증 #19](https://github.com/mrchypark/beesuh/issues/19)에 남아 있다.
