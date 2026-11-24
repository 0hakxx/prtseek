"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// 대화 입력창의 전송/줄바꿈 키. 순수 프론트엔드 선호이며 localStorage 에만 저장하고 DB 에 넣지 않으며 계정과 동기화하지 않는다.
// 그래서 브라우저를 바꾸면 다시 설정해야 한다. issue #39 참고——0.3.2 에서 Ctrl+Enter 전송을 Enter 전송으로 바꿨는데,
// 여기서 예전 키를 선택 가능한 옵션으로 되돌린다.
export type ChatSendMode = "enter" | "ctrl-enter";

const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter 전송, Shift/Ctrl+Enter 줄바꿈" },
  { value: "ctrl-enter", label: "Ctrl+Enter 전송, Enter 줄바꿈" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// 같은 탭 안의 구독자 집합. localStorage 의 storage 이벤트는 「다른」 탭에서만 트리거되므로,
// 이 페이지에서 설정을 바꾼 뒤에는 emit 으로 같은 페이지의 입력창에 알려야 한다. 그러지 않으면 새로 고쳐야 적용된다.
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// 반환하는 것은 문자열 리터럴이라 Object.is 가 값으로 비교하므로 useSyncExternalStore 가 루프에 빠지지 않는다.
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// 서버에는 localStorage 가 없으므로 먼저 기본값을 렌더링하고, hydrate 후 getSnapshot 이 바로잡는다.
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey 는 한 번의 키 입력이 전송해야 하는지 판단한다.
// isComposing / keyCode 229 는 한글/중국어 등 입력기가 글자를 조합 중이라는 뜻이므로 반드시 통과시켜야 한다. 그러지 않으면 엔터로 단어를 선택할 때 잘못 전송된다.
// enter 모드는 Shift, Ctrl, Cmd 를 제외한다: Enter 는 전송, Ctrl+Enter 는 명시적 줄바꿈용.
// ctrl-enter 모드는 Ctrl 과 Cmd(macOS)를 함께 받는다.
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey && !e.ctrlKey && !e.metaKey;
}
