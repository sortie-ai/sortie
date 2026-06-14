package linear

// queryCandidateIssues fetches issues in the configured team and active states,
// ordered by the server sort hint. The client re-sorts results by normalized
// priority, so the sort argument is only a coarse stable order for the fetch.
const queryCandidateIssues = `query CandidateIssues($teamKey: String!, $states: [String!]!, $first: Int!, $after: String) {
  issues(
    first: $first
    after: $after
    filter: { team: { key: { eq: $teamKey } }, state: { name: { in: $states } } }
    sort: [{ priority: { order: Ascending, noPriorityFirst: false } }, { createdAt: { order: Ascending } }]
  ) {
    nodes {
      id identifier title description priority branchName url createdAt updatedAt
      state { name }
      assignee { displayName name email }
      parent { id identifier }
      labels(first: 25) { nodes { name } pageInfo { hasNextPage } }
      inverseRelations(first: 25) { nodes { type issue { id identifier state { name } } } pageInfo { hasNextPage } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// queryIssuesByStates is the candidate query without the sort argument, used
// for terminal cleanup where dispatch order is irrelevant.
const queryIssuesByStates = `query IssuesByStates($teamKey: String!, $states: [String!]!, $first: Int!, $after: String) {
  issues(
    first: $first
    after: $after
    filter: { team: { key: { eq: $teamKey } }, state: { name: { in: $states } } }
  ) {
    nodes {
      id identifier title description priority branchName url createdAt updatedAt
      state { name }
      assignee { displayName name email }
      parent { id identifier }
      labels(first: 25) { nodes { name } pageInfo { hasNextPage } }
      inverseRelations(first: 25) { nodes { type issue { id identifier state { name } } } pageInfo { hasNextPage } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

// queryIssueByID fetches a single issue by UUID or human identifier, including
// the first page of comments.
const queryIssueByID = `query IssueByID($id: String!) {
  issue(id: $id) {
    id identifier title description priority branchName url createdAt updatedAt
    state { name }
    assignee { displayName name email }
    parent { id identifier }
    labels(first: 25) { nodes { name } pageInfo { hasNextPage } }
    inverseRelations(first: 25) { nodes { type issue { id identifier state { name } } } pageInfo { hasNextPage } }
    comments(first: 50, orderBy: createdAt) {
      nodes {
        id body createdAt
        user { displayName name email }
        botActor { name }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// queryIssueComments fetches a page of an issue's comments. It re-selects the
// parent issue so a not-found mid-pagination surfaces through the classifier.
const queryIssueComments = `query IssueComments($id: String!, $first: Int!, $after: String) {
  issue(id: $id) {
    comments(first: $first, after: $after, orderBy: createdAt) {
      nodes {
        id body createdAt
        user { displayName name email }
        botActor { name }
      }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// queryIssueStatesByIDs fetches issue states for a batch of UUIDs using a
// connection filter, so a missing issue is an absent node rather than an error.
const queryIssueStatesByIDs = `query IssueStatesByIDs($ids: [ID!]!, $first: Int!) {
  issues(filter: { id: { in: $ids } }, first: $first) { nodes { id state { name } } }
}`

// queryIssueStatesByNumbers fetches issue states for a batch of issue numbers
// scoped to the configured team, using a connection filter.
const queryIssueStatesByNumbers = `query IssueStatesByNumbers($teamKey: String!, $numbers: [Float!]!, $first: Int!) {
  issues(filter: { team: { key: { eq: $teamKey } }, number: { in: $numbers } }, first: $first) {
    nodes { identifier number state { name } }
  }
}`

// queryViewer is the credential-check query run at construction. It is cheap
// and classifies the API key before the first poll.
const queryViewer = `query Me {
  viewer {
    id
    name
    email
  }
}`

// queryTeamStates fetches the configured team's workflow states for the
// startup preflight and the canonical-casing cache.
const queryTeamStates = `query TeamStates($teamKey: String!) {
  teams(filter: { key: { eq: $teamKey } }, first: 1) {
    nodes {
      id
      key
      states(first: 50) {
        nodes {
          id
          name
          type
          position
        }
      }
    }
  }
}`

// queryResolveStateID resolves a workflow-state name to its UUID within the
// team that owns the issue. The eqIgnoreCase filter matches the name
// case-insensitively, so the caller's target state is sent verbatim.
const queryResolveStateID = `query ResolveStateID($issueId: String!, $stateName: String!) {
  issue(id: $issueId) {
    id
    team {
      states(filter: { name: { eqIgnoreCase: $stateName } }, first: 1) {
        nodes { id name }
      }
    }
  }
}`

// queryResolveIssueTeam resolves the UUID of the team that owns an issue. It is
// the team source for the team-scoped label create, which never runs the
// state-resolve query.
const queryResolveIssueTeam = `query ResolveIssueTeam($issueId: String!) {
  issue(id: $issueId) {
    id
    team { id }
  }
}`

// queryIssueUpdateState moves an issue to a workflow state by UUID. The
// IssueUpdateState operation name distinguishes it from the label-attach
// issueUpdate mutation under a query-substring match.
const queryIssueUpdateState = `mutation IssueUpdateState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
    issue { id state { name } }
  }
}`

// queryCommentCreate posts a markdown comment body verbatim, with no ADF-style
// wrapping.
const queryCommentCreate = `mutation CommentCreate($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment { id }
  }
}`

// queryResolveLabel resolves a label name to its UUID, case-insensitively. Each
// node carries its team so the resolver can prefer a team-scoped label over a
// workspace-scoped one.
const queryResolveLabel = `query ResolveLabel($name: String!) {
  issueLabels(filter: { name: { eqIgnoreCase: $name } }, first: 50) {
    nodes { id name team { id } }
  }
}`

// queryLabelCreate creates a team-scoped label. It always passes teamId because
// workspace-scoped label management is more likely to require elevated access.
const queryLabelCreate = `mutation LabelCreate($teamId: String!, $name: String!) {
  issueLabelCreate(input: { teamId: $teamId, name: $name }) {
    success
    issueLabel { id }
  }
}`

// queryIssueAddLabel attaches labels through addedLabelIds (append), so the
// issue's existing labels are preserved and no read-before-write of the label
// set is required. The IssueAddLabel operation name distinguishes it from the
// transition issueUpdate mutation under a query-substring match.
const queryIssueAddLabel = `mutation IssueAddLabel($id: String!, $labelIds: [String!]!) {
  issueUpdate(id: $id, input: { addedLabelIds: $labelIds }) {
    success
  }
}`
