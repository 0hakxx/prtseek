import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// 서랍/대화 상자(Sheet/Dialog)의 onInteractOutside 닫기 판정 보조.
//
// 배경: 서랍 안의 Radix 팝 레이어(Select 드롭다운, DropdownMenu, Popover 등)는 서랍 밖으로 portal 된다.
// 팝 레이어가 열린 상태에서 오버레이/서랍 밖을 눌러 접으려 하면, 이 한 번의 pointerdown 을 Select 와 Sheet 두
// DismissableLayer 가 동시에 처리한다. Select 가 먼저 닫히고 discrete 이벤트라 React 가 동기로 flush 하므로,
// Sheet 의 처리기 차례가 오면 팝 레이어의 data-state 는 이미 closed 로 바뀌어 있다 —— 「지금」 팝 레이어가 열려 있는지 감지하는 것은
// 본질적으로 신뢰할 수 없다(실측으로 확인됨).
//
// 올바른 방법: Radix 의 pointerdown 리스너는 버블 단계에 있다. 우리는 capture 단계(그보다 먼저)에서
// 「이 순간 팝 레이어가 열려 있는지」를 기록해 두고, onInteractOutside 가 이 기록 값을 읽어 닫기를 허용할지 결정한다.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture: Radix 의 버블 단계 pointerdown 처리기보다 앞서 기록
  );
}

// radixOverlayWasOpenAtPointerDown 은 「가장 최근 pointerdown 이 일어났을 때 Radix 팝 레이어가
// 열려 있었는지」를 반환한다. 서랍/대화 상자는 이를 바탕으로: 팝 레이어가 열린 상태에서 오버레이를 누르면 → 팝 레이어만 접고 자신은 닫지 않는다.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};
