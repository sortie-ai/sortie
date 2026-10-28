export function attentionNavKey(id: string): string {
  return `attention:${id}`;
}

export function activeNavKey(issueId: string): string {
  return `active:${issueId}`;
}

export function endedNavKey(issueId: string): string {
  return `ended:${issueId}`;
}
