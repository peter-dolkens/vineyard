/**
 * Model and effort picker rows for a managed session, derived from what its Claude Code reported at
 * start (ManagedInfo.models) rather than a list baked into the extension. The harness's "default" row
 * is our '' (no --model; set_model with no model).
 */

import type { ModelInfo } from './model.ts';

export type PickerOption = [value: string, label: string, title?: string];

const EFFORT_LABEL: Record<string, string> = {
  low: 'Low effort',
  medium: 'Medium effort',
  high: 'High effort',
  xhigh: 'Extra-high effort',
  max: 'Max effort',
};

/** The full range the control protocol accepts; used only until the harness has said what the model supports. */
export const ALL_EFFORTS: PickerOption[] = [['', 'Default effort'], ...Object.entries(EFFORT_LABEL)];

/** The picker value for a harness row: Claude Code's "default" is our empty string. */
export function pickerValue(m: ModelInfo): string {
  return m.value === 'default' ? '' : m.value;
}

export function modelOptions(models: ModelInfo[] | undefined): PickerOption[] {
  if (!models?.length) return [['', 'Default model']];
  return models.map((m) => [pickerValue(m), m.displayName, m.description]);
}

/**
 * The picker row for a session's current model: the row whose value we asked for, else the first row
 * that resolves to the wire id the session reported (with or without a context-window suffix), else
 * the raw id so the picker can still show it.
 */
export function selectedModel(models: ModelInfo[] | undefined, current: string): string {
  if (!current || !models?.length) return current;
  const exact = models.find((m) => m.value === current || pickerValue(m) === current);
  if (exact) return pickerValue(exact);
  const bare = (s: string) => s.replace(/\[.*\]$/, '');
  const resolved = models.find((m) => m.resolvedModel === current) ?? models.find((m) => m.resolvedModel && bare(m.resolvedModel) === bare(current));
  return resolved ? pickerValue(resolved) : current;
}

/** Effort rows for the selected model: only what it supports, just "default" when it takes none. */
export function effortOptions(models: ModelInfo[] | undefined, selected: string): PickerOption[] {
  const row = models?.find((m) => pickerValue(m) === selected);
  if (!row) return ALL_EFFORTS;
  if (!row.supportedEffortLevels?.length) return [['', 'Default effort']];
  return [['', 'Default effort'], ...row.supportedEffortLevels.map((l): PickerOption => [l, EFFORT_LABEL[l] ?? l])];
}
