import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "prtseek",
  version: packageJson.version,
  copyright: `© ${currentYear}, prtseek.`,
  meta: {
    title: "prtseek — 연구 터미널",
    description: "prtseek · 분석, 연구, 검증 워크벤치",
  },
};
