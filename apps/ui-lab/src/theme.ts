import type { TextStyle, ViewStyle } from 'react-native';

export const colors = {
  canvas: '#08090c',
  canvasSoft: '#0d0f14',
  panel: '#11141a',
  panelHigh: '#171b23',
  panelActive: '#1d232c',
  border: '#252b36',
  borderStrong: '#343c4a',
  text: '#f4f6f9',
  textSoft: '#c1c7d0',
  muted: '#858e9e',
  faint: '#5d6572',
  accent: '#c8f26b',
  accentInk: '#132000',
  cyan: '#6dd5e7',
  cyanSoft: '#18363e',
  amber: '#f3b867',
  amberSoft: '#3b2c18',
  red: '#f08080',
  redSoft: '#3b2024',
  green: '#7fda9a',
  greenSoft: '#183325',
  purple: '#b7a3f7',
  purpleSoft: '#2c2545',
  terminal: '#07090b',
};

export const radius = {
  sm: 8,
  md: 12,
  lg: 18,
  xl: 24,
  pill: 999,
};

export const shadow: ViewStyle = {
  shadowColor: '#000',
  shadowOpacity: 0.28,
  shadowRadius: 18,
  shadowOffset: { width: 0, height: 8 },
};

export const type = {
  title: { color: colors.text, fontSize: 24, lineHeight: 30, fontWeight: '700' } satisfies TextStyle,
  heading: { color: colors.text, fontSize: 17, lineHeight: 22, fontWeight: '700' } satisfies TextStyle,
  body: { color: colors.textSoft, fontSize: 15, lineHeight: 22 } satisfies TextStyle,
  label: { color: colors.text, fontSize: 13, lineHeight: 18, fontWeight: '600' } satisfies TextStyle,
  meta: { color: colors.muted, fontSize: 12, lineHeight: 16 } satisfies TextStyle,
  mono: {
    color: colors.textSoft,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
    fontSize: 12,
    lineHeight: 18,
  } satisfies TextStyle,
};
