import * as Haptics from 'expo-haptics';
import {
  AlertTriangle,
  ArrowLeft,
  ArrowUp,
  Bot,
  Check,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  Circle,
  Clock3,
  FileCode2,
  FolderGit2,
  GitBranch,
  Laptop,
  ListTree,
  MessageSquare,
  MoreHorizontal,
  Paperclip,
  Play,
  Radio,
  RotateCw,
  Server,
  ShieldCheck,
  Sparkles,
  Square,
  TerminalSquare,
  UserRound,
  WifiOff,
  Wrench,
  XCircle,
} from 'lucide-react-native';
import React from 'react';
import {
  ActivityIndicator,
  Platform,
  Pressable,
  ScrollView,
  StyleSheet,
  Text,
  TextInput,
  useWindowDimensions,
  View,
} from 'react-native';

import type { Candidate, Node, SessionState, SessionSummary, TimelineItem, Workspace } from './model';
import { nodeFor, timeline, workspaceFor } from './model';
import { colors, radius, shadow, type } from './theme';

function tap(): void {
  if (Platform.OS !== 'web') void Haptics.selectionAsync();
}

export function PressableScale({
  children,
  onPress,
  accessibilityLabel,
  style,
  testID,
}: {
  children: React.ReactNode;
  onPress?: () => void;
  accessibilityLabel: string;
  style?: object;
  testID?: string;
}) {
  return (
    <Pressable
      accessibilityRole="button"
      accessibilityLabel={accessibilityLabel}
      testID={testID}
      onPress={() => {
        tap();
        onPress?.();
      }}
      style={({ pressed }) => [style, pressed && styles.pressed]}
    >
      {children}
    </Pressable>
  );
}

const stateTone: Record<SessionState, { label: string; color: string; background: string }> = {
  working: { label: 'Working', color: colors.accent, background: '#26351b' },
  'needs-input': { label: 'Needs you', color: colors.amber, background: colors.amberSoft },
  idle: { label: 'Idle', color: colors.muted, background: colors.panelHigh },
  done: { label: 'Done', color: colors.green, background: colors.greenSoft },
  interrupted: { label: 'Interrupted', color: colors.red, background: colors.redSoft },
};

export function SessionPill({ state, compact = false }: { state: SessionState; compact?: boolean }) {
  const tone = stateTone[state];
  return (
    <View style={[styles.pill, { backgroundColor: tone.background }, compact && styles.pillCompact]}>
      <View style={[styles.pillDot, { backgroundColor: tone.color }]} />
      <Text style={[styles.pillText, { color: tone.color }]}>{tone.label}</Text>
    </View>
  );
}

export function Eyebrow({ children }: { children: React.ReactNode }) {
  return <Text style={styles.eyebrow}>{children}</Text>;
}

export function SectionTitle({
  title,
  count,
  action,
  onAction,
}: {
  title: string;
  count?: string;
  action?: string;
  onAction?: () => void;
}) {
  return (
    <View style={styles.sectionTitleRow}>
      <View style={styles.sectionTitleLeft}>
        <Text style={type.heading}>{title}</Text>
        {count ? <Text style={styles.sectionCount}>{count}</Text> : null}
      </View>
      {action ? (
        <PressableScale accessibilityLabel={action} onPress={onAction} style={styles.textAction}>
          <Text style={styles.textActionLabel}>{action}</Text>
          <ChevronRight size={15} color={colors.muted} />
        </PressableScale>
      ) : null}
    </View>
  );
}

export function LabSwitcher({ candidate, onChange }: { candidate: Candidate; onChange: (candidate: Candidate) => void }) {
  const { width } = useWindowDimensions();
  const mobile = width < 620;
  const choices: { id: Candidate; label: string; source: string }[] = [
    { id: 'fleet', label: 'Fleet', source: 'Happy' },
    { id: 'focus', label: 'Focus', source: 'OpenCode' },
    { id: 'ops', label: 'Ops', source: 'Paseo · Orca' },
  ];
  return (
    <View style={[styles.labBar, mobile && styles.labBarMobile]}>
      <View style={[styles.labBrand, mobile && styles.labBrandMobile]}>
        <View style={styles.labMark}><Sparkles size={14} color={colors.accentInk} strokeWidth={2.5} /></View>
        <View>
          <Text style={styles.labTitle}>agentd UI lab</Text>
          <Text style={styles.labSubtitle}>fixture-driven comparison</Text>
        </View>
      </View>
      <View style={[styles.segmented, mobile && styles.segmentedMobile]} accessibilityRole="tablist">
        {choices.map((choice) => {
          const selected = choice.id === candidate;
          return (
            <PressableScale
              key={choice.id}
              accessibilityLabel={`${choice.label} candidate inspired by ${choice.source}`}
              testID={`candidate-${choice.id}`}
              onPress={() => onChange(choice.id)}
              style={[styles.segment, selected && styles.segmentActive]}
            >
              <Text style={[styles.segmentLabel, selected && styles.segmentLabelActive]}>{choice.label}</Text>
              <Text style={[styles.segmentSource, selected && styles.segmentSourceActive]}>{choice.source}</Text>
            </PressableScale>
          );
        })}
      </View>
    </View>
  );
}

export function SourceNote({ children }: { children: React.ReactNode }) {
  return (
    <View style={styles.sourceNote}>
      <ShieldCheck size={14} color={colors.muted} />
      <Text style={styles.sourceNoteText}>{children}</Text>
    </View>
  );
}

export function NodeCard({ node, selected, onPress }: { node: Node; selected?: boolean; onPress?: () => void }) {
  const offline = node.state === 'offline';
  const busy = node.state === 'busy';
  return (
    <PressableScale
      accessibilityLabel={`${node.name}, ${node.state}, ${node.activeRuns} active runs`}
      onPress={onPress}
      style={[styles.nodeCard, selected && styles.nodeCardSelected, offline && styles.cardDimmed]}
    >
      <View style={styles.nodeCardTop}>
        <View style={[styles.nodeIcon, selected && styles.nodeIconSelected]}>
          {node.platform.startsWith('Linux') ? (
            <Server size={19} color={selected ? colors.accentInk : colors.textSoft} />
          ) : (
            <Laptop size={19} color={selected ? colors.accentInk : colors.textSoft} />
          )}
        </View>
        <View style={styles.nodeStateLine}>
          {offline ? <WifiOff size={13} color={colors.red} /> : <Radio size={13} color={busy ? colors.accent : colors.green} />}
          <Text style={[styles.nodeStateText, { color: offline ? colors.red : busy ? colors.accent : colors.green }]}>
            {offline ? node.lastSeen : busy ? `${node.activeRuns} active` : 'Ready'}
          </Text>
        </View>
      </View>
      <Text style={styles.nodeName}>{node.name}</Text>
      <Text style={styles.nodeLocation}>{node.location}</Text>
      <View style={styles.nodeMetaRow}>
        <Text style={styles.nodeMeta}>{node.platform}</Text>
        <View style={styles.metaSeparator} />
        <Text style={styles.nodeMeta}>{node.latency}</Text>
      </View>
    </PressableScale>
  );
}

export function SessionRow({ session, onPress, emphasized = false }: { session: SessionSummary; onPress?: () => void; emphasized?: boolean }) {
  const node = nodeFor(session.nodeId);
  const workspace = workspaceFor(session.workspaceId);
  return (
    <PressableScale
      accessibilityLabel={`Open session ${session.title}`}
      testID={`session-${session.id}`}
      onPress={onPress}
      style={[styles.sessionRow, emphasized && styles.sessionRowEmphasized]}
    >
      <View style={[styles.sessionGlyph, session.state === 'needs-input' && styles.sessionGlyphAttention]}>
        <Bot size={20} color={session.state === 'needs-input' ? colors.amber : colors.textSoft} />
      </View>
      <View style={styles.sessionCopy}>
        <View style={styles.sessionTitleLine}>
          <Text style={styles.sessionTitle} numberOfLines={1}>{session.title}</Text>
          <Text style={styles.sessionTime}>{session.updated}</Text>
        </View>
        <Text style={styles.sessionSummary} numberOfLines={1}>{session.summary}</Text>
        <View style={styles.sessionMetaLine}>
          <SessionPill state={session.state} compact />
          <Text style={styles.sessionMetaText}>{workspace.name}</Text>
          <View style={styles.metaSeparator} />
          <Text style={styles.sessionMetaText}>{node.name}</Text>
          <View style={styles.metaSeparator} />
          <Text style={styles.sessionMetaText}>{session.harness}</Text>
        </View>
      </View>
      <ChevronRight size={18} color={colors.faint} />
    </PressableScale>
  );
}

function ToolStatus({ status }: { status: 'running' | 'complete' | 'error' }) {
  if (status === 'running') return <ActivityIndicator size="small" color={colors.accent} />;
  if (status === 'error') return <XCircle size={16} color={colors.red} />;
  return <CheckCircle2 size={16} color={colors.green} />;
}

export function ToolCard({ item }: { item: Extract<TimelineItem, { kind: 'tool' }> }) {
  const [expanded, setExpanded] = React.useState(item.status === 'running');
  return (
    <View style={[styles.activityCard, item.status === 'running' && styles.activityCardRunning]}>
      <PressableScale
        accessibilityLabel={`${expanded ? 'Collapse' : 'Expand'} ${item.title}`}
        onPress={() => setExpanded((value) => !value)}
        style={styles.activityHeader}
      >
        <View style={styles.activityIcon}><Wrench size={16} color={colors.cyan} /></View>
        <View style={styles.activityCopy}>
          <View style={styles.activityTitleLine}>
            <Text style={styles.activityKind}>{item.tool}</Text>
            {item.duration ? <Text style={styles.activityDuration}>{item.duration}</Text> : null}
          </View>
          <Text style={styles.activityTitle}>{item.title}</Text>
          <Text style={styles.activityDetail} numberOfLines={expanded ? undefined : 1}>{item.detail}</Text>
        </View>
        <ToolStatus status={item.status} />
        {expanded ? <ChevronDown size={16} color={colors.muted} /> : <ChevronRight size={16} color={colors.muted} />}
      </PressableScale>
      {expanded && item.output ? (
        <View style={styles.toolOutput}>
          {item.output.map((line) => (
            <View key={line} style={styles.outputLine}>
              <Circle size={5} fill={colors.faint} color={colors.faint} />
              <Text style={styles.outputText}>{line}</Text>
            </View>
          ))}
        </View>
      ) : null}
    </View>
  );
}

export function SubagentCard({ item }: { item: Extract<TimelineItem, { kind: 'subagent' }> }) {
  return (
    <View style={styles.subagentCard}>
      <View style={styles.subagentRail} />
      <View style={styles.subagentIcon}><ListTree size={16} color={colors.purple} /></View>
      <View style={styles.subagentCopy}>
        <View style={styles.subagentTitleLine}>
          <Text style={styles.subagentName}>{item.name}</Text>
          <Text style={styles.subagentState}>{item.status === 'done' ? 'Done' : 'Working'}</Text>
        </View>
        <Text style={styles.subagentTask}>{item.task}</Text>
        <Text style={styles.subagentProgress}>{item.progress}</Text>
      </View>
    </View>
  );
}

export function DiffCard({ item }: { item: Extract<TimelineItem, { kind: 'diff' }> }) {
  const [expanded, setExpanded] = React.useState(true);
  return (
    <View style={styles.diffCard}>
      <PressableScale
        accessibilityLabel={`${expanded ? 'Collapse' : 'Expand'} diff for ${item.file}`}
        onPress={() => setExpanded((value) => !value)}
        style={styles.diffHeader}
      >
        <View style={styles.diffFileIcon}><FileCode2 size={17} color={colors.textSoft} /></View>
        <View style={styles.diffHeaderCopy}>
          <Text style={styles.diffFile} numberOfLines={1}>{item.file}</Text>
          <Text style={styles.diffStats}><Text style={{ color: colors.green }}>+{item.additions}</Text>  <Text style={{ color: colors.red }}>−{item.deletions}</Text></Text>
        </View>
        {expanded ? <ChevronDown size={17} color={colors.muted} /> : <ChevronRight size={17} color={colors.muted} />}
      </PressableScale>
      {expanded ? (
        <ScrollView horizontal showsHorizontalScrollIndicator={false} style={styles.diffBody}>
          <View style={styles.diffLines}>
            {item.lines.map((line, index) => {
              const prefix = line.type === 'add' ? '+' : line.type === 'remove' ? '−' : ' ';
              return (
                <View
                  key={`${index}-${line.value}`}
                  style={[
                    styles.diffLine,
                    line.type === 'add' && styles.diffLineAdd,
                    line.type === 'remove' && styles.diffLineRemove,
                  ]}
                >
                  <Text style={[styles.diffPrefix, line.type === 'add' && { color: colors.green }, line.type === 'remove' && { color: colors.red }]}>{prefix}</Text>
                  <Text style={styles.diffCode}>{line.value}</Text>
                </View>
              );
            })}
          </View>
        </ScrollView>
      ) : null}
    </View>
  );
}

export function InteractionCard({ item }: { item: Extract<TimelineItem, { kind: 'interaction' }> }) {
  const [selected, setSelected] = React.useState<string>();
  return (
    <View style={styles.interactionCard} testID="interaction-card">
      <View style={styles.interactionTop}>
        <View style={styles.interactionIcon}><MessageSquare size={17} color={colors.amber} /></View>
        <View style={styles.interactionCopy}>
          <Text style={styles.interactionEyebrow}>Decision needed</Text>
          <Text style={styles.interactionTitle}>{item.title}</Text>
        </View>
      </View>
      <Text style={styles.interactionBody}>{item.body}</Text>
      <View style={styles.optionList}>
        {item.options.map((option) => {
          const active = selected === option.id;
          return (
            <PressableScale
              key={option.id}
              accessibilityLabel={`Choose ${option.label}`}
              testID={`interaction-${option.id}`}
              onPress={() => setSelected(option.id)}
              style={[styles.option, active && styles.optionSelected]}
            >
              <View style={[styles.optionRadio, active && styles.optionRadioSelected]}>
                {active ? <Check size={13} color={colors.accentInk} strokeWidth={3} /> : null}
              </View>
              <View style={styles.optionCopy}>
                <Text style={[styles.optionLabel, active && styles.optionLabelSelected]}>{option.label}</Text>
                <Text style={styles.optionDetail}>{option.detail}</Text>
              </View>
            </PressableScale>
          );
        })}
      </View>
      <PressableScale
        accessibilityLabel="Submit recovery policy"
        onPress={() => undefined}
        style={[styles.submitDecision, !selected && styles.submitDecisionDisabled]}
      >
        <Text style={[styles.submitDecisionText, !selected && styles.submitDecisionTextDisabled]}>{selected ? 'Use this policy' : 'Choose a policy'}</Text>
        <ArrowUp size={16} color={selected ? colors.accentInk : colors.faint} />
      </PressableScale>
    </View>
  );
}

export function Timeline({ extraMessages = [] }: { extraMessages?: string[] }) {
  return (
    <View style={styles.timeline}>
      {timeline.map((item) => {
        if (item.kind === 'user') {
          return (
            <View key={item.id} style={styles.userMessageRow}>
              <View style={styles.userMessage}>
                <View style={styles.messageAuthorLine}><UserRound size={14} color={colors.muted} /><Text style={styles.messageAuthor}>You</Text><Text style={styles.messageTime}>{item.at}</Text></View>
                <Text style={styles.userMessageText}>{item.text}</Text>
              </View>
            </View>
          );
        }
        if (item.kind === 'assistant') {
          return (
            <View key={item.id} style={styles.assistantMessage}>
              <View style={styles.messageAuthorLine}>
                <View style={styles.assistantAvatar}><Bot size={13} color={colors.accentInk} /></View>
                <Text style={styles.assistantAuthor}>OMP</Text>
                <Text style={styles.messageTime}>{item.at}</Text>
                {item.streaming ? <View style={styles.streamingDot} /> : null}
              </View>
              <Text style={styles.assistantMessageText}>{item.text}</Text>
            </View>
          );
        }
        if (item.kind === 'tool') return <ToolCard key={item.id} item={item} />;
        if (item.kind === 'diff') return <DiffCard key={item.id} item={item} />;
        if (item.kind === 'interaction') return <InteractionCard key={item.id} item={item} />;
        if (item.kind === 'subagent') return <SubagentCard key={item.id} item={item} />;
        return (
          <View key={item.id} style={styles.errorCard}>
            <AlertTriangle size={17} color={colors.red} />
            <View><Text style={styles.errorTitle}>{item.title}</Text><Text style={styles.errorDetail}>{item.detail}</Text></View>
          </View>
        );
      })}
      {extraMessages.map((message, index) => (
        <View key={`${index}-${message}`} style={styles.userMessageRow}>
          <View style={styles.userMessage}>
            <View style={styles.messageAuthorLine}><UserRound size={14} color={colors.muted} /><Text style={styles.messageAuthor}>You</Text><Text style={styles.messageTime}>now</Text></View>
            <Text style={styles.userMessageText}>{message}</Text>
          </View>
        </View>
      ))}
    </View>
  );
}

export function Composer({ onSend, placeholder = 'Message or steer the agent…' }: { onSend: (message: string) => void; placeholder?: string }) {
  const [value, setValue] = React.useState('');
  const submit = () => {
    const message = value.trim();
    if (!message) return;
    tap();
    onSend(message);
    setValue('');
  };
  return (
    <View style={styles.composerWrap}>
      <View style={styles.composer}>
        <TextInput
          accessibilityLabel="Message agent"
          testID="composer-input"
          multiline
          placeholder={placeholder}
          placeholderTextColor={colors.faint}
          value={value}
          onChangeText={setValue}
          style={styles.composerInput}
          maxLength={4000}
        />
        <View style={styles.composerActions}>
          <PressableScale accessibilityLabel="Attach file" style={styles.composerIconButton}>
            <Paperclip size={18} color={colors.muted} />
          </PressableScale>
          <View style={styles.composerMode}><Text style={styles.composerModeText}>Steer</Text><ChevronDown size={13} color={colors.muted} /></View>
          <PressableScale
            accessibilityLabel="Send message"
            testID="composer-send"
            onPress={submit}
            style={[styles.sendButton, !value.trim() && styles.sendButtonDisabled]}
          >
            <ArrowUp size={18} color={value.trim() ? colors.accentInk : colors.faint} strokeWidth={2.5} />
          </PressableScale>
        </View>
      </View>
      <Text style={styles.composerHint}>Fixture mode · messages stay on this device</Text>
    </View>
  );
}

export function SessionHeader({ session, onBack }: { session: SessionSummary; onBack?: () => void }) {
  const node = nodeFor(session.nodeId);
  const workspace = workspaceFor(session.workspaceId);
  return (
    <View style={styles.sessionHeader}>
      <PressableScale accessibilityLabel="Back to fleet" onPress={onBack} style={styles.headerIconButton}>
        <ArrowLeft size={20} color={colors.textSoft} />
      </PressableScale>
      <View style={styles.sessionHeaderCopy}>
        <Text style={styles.sessionHeaderTitle} numberOfLines={1}>{session.title}</Text>
        <View style={styles.sessionHeaderMeta}>
          <FolderGit2 size={13} color={colors.muted} />
          <Text style={styles.sessionHeaderMetaText}>{workspace.name}</Text>
          <View style={styles.metaSeparator} />
          <Radio size={12} color={colors.accent} />
          <Text style={styles.sessionHeaderMetaText}>{node.name}</Text>
        </View>
      </View>
      <SessionPill state={session.state} compact />
      <PressableScale accessibilityLabel="More session actions" style={styles.headerIconButton}>
        <MoreHorizontal size={20} color={colors.textSoft} />
      </PressableScale>
    </View>
  );
}

export function ReviewRail({ session, workspace }: { session: SessionSummary; workspace: Workspace }) {
  return (
    <View style={styles.reviewRail}>
      <Eyebrow>RUN CONTEXT</Eyebrow>
      <Text style={styles.reviewRailTitle}>{workspace.name}</Text>
      <View style={styles.reviewMetric}><GitBranch size={14} color={colors.muted} /><Text style={styles.reviewMetricLabel}>{workspace.branch}</Text></View>
      <View style={styles.reviewMetric}><Clock3 size={14} color={colors.muted} /><Text style={styles.reviewMetricLabel}>Running 3m 18s</Text></View>
      <View style={styles.reviewDivider} />
      <Eyebrow>CHANGES</Eyebrow>
      <View style={styles.changeSummary}>
        <View><Text style={styles.changeNumber}>{session.changedFiles}</Text><Text style={styles.changeLabel}>files</Text></View>
        <View><Text style={[styles.changeNumber, { color: colors.green }]}>+{session.additions}</Text><Text style={styles.changeLabel}>added</Text></View>
        <View><Text style={[styles.changeNumber, { color: colors.red }]}>−{session.deletions}</Text><Text style={styles.changeLabel}>removed</Text></View>
      </View>
      <View style={styles.changedFile}><FileCode2 size={15} color={colors.textSoft} /><Text style={styles.changedFileName}>internal/fleet/service.go</Text><Text style={styles.changedFileStats}>+14 −3</Text></View>
      <View style={styles.changedFile}><FileCode2 size={15} color={colors.textSoft} /><Text style={styles.changedFileName}>internal/sessions/manager.go</Text><Text style={styles.changedFileStats}>+42 −8</Text></View>
      <View style={styles.changedFile}><FileCode2 size={15} color={colors.textSoft} /><Text style={styles.changedFileName}>internal/fleet/service_test.go</Text><Text style={styles.changedFileStats}>+28 −6</Text></View>
      <View style={styles.reviewDivider} />
      <PressableScale accessibilityLabel="Abort fixture run" style={styles.abortButton}>
        <Square size={14} fill={colors.red} color={colors.red} />
        <Text style={styles.abortButtonText}>Abort run</Text>
      </PressableScale>
    </View>
  );
}

export function TerminalPanel({ lines, command }: { lines: string[]; command?: string }) {
  return (
    <View style={styles.terminalPanel}>
      <View style={styles.terminalTopbar}>
        <View style={styles.terminalLights}><View style={[styles.terminalLight, { backgroundColor: '#ff6b68' }]} /><View style={[styles.terminalLight, { backgroundColor: '#f7c55b' }]} /><View style={[styles.terminalLight, { backgroundColor: '#61d36e' }]} /></View>
        <View style={styles.terminalTitle}><TerminalSquare size={14} color={colors.muted} /><Text style={styles.terminalTitleText}>agentd-node · Studio</Text></View>
        <Text style={styles.terminalBadge}>screen fallback</Text>
      </View>
      <ScrollView horizontal showsHorizontalScrollIndicator={false} style={styles.terminalBody}>
        <View>
          {lines.map((line, index) => <Text key={`${index}-${line}`} style={[styles.terminalLine, line.includes('lost') && { color: colors.amber }, line.includes('restored') && { color: colors.green }]}>{line}</Text>)}
          {command ? <Text style={[styles.terminalLine, { color: colors.cyan }]}>$ {command}</Text> : null}
          <View style={styles.cursor} />
        </View>
      </ScrollView>
    </View>
  );
}

export const chromeIcons = {
  Play,
  RotateCw,
  TerminalSquare,
};

const styles = StyleSheet.create({
  pressed: { opacity: 0.72, transform: [{ scale: 0.992 }] },
  pill: { flexDirection: 'row', alignItems: 'center', gap: 6, borderRadius: radius.pill, paddingHorizontal: 10, paddingVertical: 6 },
  pillCompact: { paddingHorizontal: 8, paddingVertical: 4 },
  pillDot: { width: 6, height: 6, borderRadius: 3 },
  pillText: { fontSize: 11, lineHeight: 14, fontWeight: '700' },
  eyebrow: { color: colors.muted, fontSize: 10, lineHeight: 14, fontWeight: '700', letterSpacing: 1.2 },
  sectionTitleRow: { minHeight: 32, flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', gap: 12 },
  sectionTitleLeft: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  sectionCount: { color: colors.muted, fontSize: 12, fontWeight: '600', backgroundColor: colors.panelHigh, paddingHorizontal: 7, paddingVertical: 3, borderRadius: radius.pill },
  textAction: { minHeight: 40, flexDirection: 'row', alignItems: 'center', gap: 2, paddingHorizontal: 4 },
  textActionLabel: { color: colors.muted, fontSize: 13, fontWeight: '600' },
  labBar: { minHeight: 74, flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', gap: 16, paddingHorizontal: 18, paddingVertical: 10, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: colors.border, backgroundColor: colors.canvasSoft },
  labBarMobile: { minHeight: 112, flexDirection: 'column', alignItems: 'stretch', gap: 8, paddingHorizontal: 10, paddingVertical: 8 },
  labBrand: { flexDirection: 'row', alignItems: 'center', gap: 10, minWidth: 150 },
  labBrandMobile: { minWidth: 0, paddingHorizontal: 2 },
  labMark: { width: 30, height: 30, borderRadius: 10, backgroundColor: colors.accent, alignItems: 'center', justifyContent: 'center' },
  labTitle: { color: colors.text, fontSize: 13, fontWeight: '700' },
  labSubtitle: { color: colors.muted, fontSize: 10, marginTop: 1 },
  segmented: { flex: 1, maxWidth: 520, flexDirection: 'row', alignItems: 'stretch', gap: 4, borderRadius: 14, padding: 4, backgroundColor: colors.canvas },
  segmentedMobile: { flex: 0, flexBasis: 54, width: '100%' },
  segment: { flex: 1, minHeight: 46, borderRadius: 10, alignItems: 'center', justifyContent: 'center', paddingHorizontal: 8, paddingVertical: 6 },
  segmentActive: { backgroundColor: colors.panelHigh, borderWidth: StyleSheet.hairlineWidth, borderColor: colors.borderStrong },
  segmentLabel: { color: colors.muted, fontSize: 12, fontWeight: '700' },
  segmentLabelActive: { color: colors.text },
  segmentSource: { color: colors.faint, fontSize: 9, marginTop: 2 },
  segmentSourceActive: { color: colors.accent },
  sourceNote: { flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 7, minHeight: 34, paddingHorizontal: 14 },
  sourceNoteText: { color: colors.muted, fontSize: 10, lineHeight: 14, textAlign: 'center' },
  nodeCard: { width: 222, minHeight: 154, borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, padding: 15, ...shadow },
  nodeCardSelected: { borderColor: colors.accent, backgroundColor: colors.panelActive },
  cardDimmed: { opacity: 0.58 },
  nodeCardTop: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', marginBottom: 15 },
  nodeIcon: { width: 36, height: 36, borderRadius: 11, alignItems: 'center', justifyContent: 'center', backgroundColor: colors.panelHigh },
  nodeIconSelected: { backgroundColor: colors.accent },
  nodeStateLine: { flexDirection: 'row', alignItems: 'center', gap: 5 },
  nodeStateText: { fontSize: 11, fontWeight: '700' },
  nodeName: { color: colors.text, fontSize: 17, fontWeight: '700' },
  nodeLocation: { color: colors.muted, fontSize: 12, marginTop: 3 },
  nodeMetaRow: { flexDirection: 'row', alignItems: 'center', gap: 7, marginTop: 15 },
  nodeMeta: { color: colors.muted, fontSize: 10 },
  metaSeparator: { width: 3, height: 3, borderRadius: 2, backgroundColor: colors.faint },
  sessionRow: { minHeight: 96, flexDirection: 'row', alignItems: 'center', gap: 13, paddingHorizontal: 14, paddingVertical: 12, borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel },
  sessionRowEmphasized: { borderColor: '#5a492d', backgroundColor: '#18150f' },
  sessionGlyph: { width: 42, height: 42, borderRadius: 14, alignItems: 'center', justifyContent: 'center', backgroundColor: colors.panelHigh },
  sessionGlyphAttention: { backgroundColor: colors.amberSoft },
  sessionCopy: { flex: 1, minWidth: 0 },
  sessionTitleLine: { flexDirection: 'row', alignItems: 'center', gap: 10 },
  sessionTitle: { flex: 1, color: colors.text, fontSize: 14, lineHeight: 19, fontWeight: '700' },
  sessionTime: { color: colors.faint, fontSize: 10 },
  sessionSummary: { color: colors.muted, fontSize: 12, lineHeight: 17, marginTop: 3 },
  sessionMetaLine: { minHeight: 22, flexDirection: 'row', alignItems: 'center', gap: 7, marginTop: 6 },
  sessionMetaText: { color: colors.muted, fontSize: 10 },
  activityCard: { borderRadius: radius.md, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, overflow: 'hidden' },
  activityCardRunning: { borderColor: '#45552b' },
  activityHeader: { flexDirection: 'row', alignItems: 'center', gap: 10, padding: 12 },
  activityIcon: { width: 30, height: 30, borderRadius: 9, backgroundColor: colors.cyanSoft, alignItems: 'center', justifyContent: 'center' },
  activityCopy: { flex: 1, minWidth: 0 },
  activityTitleLine: { flexDirection: 'row', alignItems: 'center', gap: 7 },
  activityKind: { color: colors.cyan, fontSize: 10, lineHeight: 14, fontWeight: '800', textTransform: 'uppercase', letterSpacing: 0.7 },
  activityDuration: { color: colors.faint, fontSize: 9 },
  activityTitle: { color: colors.text, fontSize: 13, lineHeight: 18, fontWeight: '600' },
  activityDetail: { color: colors.muted, fontSize: 11, lineHeight: 15, marginTop: 2 },
  toolOutput: { gap: 7, paddingHorizontal: 14, paddingTop: 2, paddingBottom: 13, borderTopWidth: StyleSheet.hairlineWidth, borderTopColor: colors.border },
  outputLine: { flexDirection: 'row', alignItems: 'flex-start', gap: 8, paddingTop: 7 },
  outputText: { flex: 1, color: colors.textSoft, fontSize: 11, lineHeight: 16 },
  subagentCard: { position: 'relative', flexDirection: 'row', alignItems: 'flex-start', gap: 11, padding: 12, borderRadius: radius.md, backgroundColor: colors.panel },
  subagentRail: { position: 'absolute', left: 0, top: 10, bottom: 10, width: 3, borderRadius: 2, backgroundColor: colors.purple },
  subagentIcon: { width: 30, height: 30, borderRadius: 9, backgroundColor: colors.purpleSoft, alignItems: 'center', justifyContent: 'center' },
  subagentCopy: { flex: 1 },
  subagentTitleLine: { flexDirection: 'row', justifyContent: 'space-between', gap: 10 },
  subagentName: { color: colors.purple, fontSize: 11, fontWeight: '800' },
  subagentState: { color: colors.green, fontSize: 10, fontWeight: '700' },
  subagentTask: { color: colors.text, fontSize: 12, lineHeight: 17, fontWeight: '600', marginTop: 2 },
  subagentProgress: { color: colors.muted, fontSize: 11, lineHeight: 16, marginTop: 3 },
  diffCard: { borderRadius: radius.md, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, overflow: 'hidden' },
  diffHeader: { minHeight: 54, flexDirection: 'row', alignItems: 'center', gap: 10, paddingHorizontal: 12 },
  diffFileIcon: { width: 30, height: 30, borderRadius: 9, backgroundColor: colors.panelHigh, alignItems: 'center', justifyContent: 'center' },
  diffHeaderCopy: { flex: 1, minWidth: 0 },
  diffFile: { color: colors.text, fontSize: 12, lineHeight: 16, fontWeight: '600' },
  diffStats: { color: colors.muted, fontSize: 10, marginTop: 2 },
  diffBody: { maxHeight: 210, borderTopWidth: StyleSheet.hairlineWidth, borderTopColor: colors.border, backgroundColor: '#0b0d11' },
  diffLines: { minWidth: 620, paddingVertical: 7 },
  diffLine: { minHeight: 24, flexDirection: 'row', alignItems: 'center', paddingHorizontal: 10 },
  diffLineAdd: { backgroundColor: 'rgba(90, 190, 118, 0.13)' },
  diffLineRemove: { backgroundColor: 'rgba(225, 105, 105, 0.12)' },
  diffPrefix: { width: 20, color: colors.faint, fontFamily: 'monospace', fontSize: 11 },
  diffCode: { color: colors.textSoft, fontFamily: 'monospace', fontSize: 11 },
  interactionCard: { borderRadius: radius.lg, borderWidth: 1, borderColor: '#604b2a', backgroundColor: '#17130d', padding: 14, ...shadow },
  interactionTop: { flexDirection: 'row', alignItems: 'center', gap: 11 },
  interactionIcon: { width: 34, height: 34, borderRadius: 11, backgroundColor: colors.amberSoft, alignItems: 'center', justifyContent: 'center' },
  interactionCopy: { flex: 1 },
  interactionEyebrow: { color: colors.amber, fontSize: 9, lineHeight: 13, fontWeight: '800', textTransform: 'uppercase', letterSpacing: 0.9 },
  interactionTitle: { color: colors.text, fontSize: 15, lineHeight: 20, fontWeight: '700', marginTop: 1 },
  interactionBody: { color: colors.textSoft, fontSize: 12, lineHeight: 18, marginTop: 11 },
  optionList: { gap: 7, marginTop: 12 },
  option: { flexDirection: 'row', alignItems: 'flex-start', gap: 10, minHeight: 58, borderRadius: radius.md, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, padding: 10 },
  optionSelected: { borderColor: colors.accent, backgroundColor: '#192111' },
  optionRadio: { width: 20, height: 20, borderRadius: 10, borderWidth: 1, borderColor: colors.borderStrong, alignItems: 'center', justifyContent: 'center', marginTop: 1 },
  optionRadioSelected: { borderColor: colors.accent, backgroundColor: colors.accent },
  optionCopy: { flex: 1 },
  optionLabel: { color: colors.textSoft, fontSize: 12, lineHeight: 16, fontWeight: '700' },
  optionLabelSelected: { color: colors.text },
  optionDetail: { color: colors.muted, fontSize: 10, lineHeight: 15, marginTop: 2 },
  submitDecision: { minHeight: 44, flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 7, borderRadius: radius.md, backgroundColor: colors.accent, marginTop: 12 },
  submitDecisionDisabled: { backgroundColor: colors.panelHigh },
  submitDecisionText: { color: colors.accentInk, fontSize: 12, fontWeight: '800' },
  submitDecisionTextDisabled: { color: colors.faint },
  timeline: { gap: 12 },
  userMessageRow: { alignItems: 'flex-end' },
  userMessage: { width: '92%', maxWidth: 640, borderRadius: 18, borderTopRightRadius: 6, backgroundColor: colors.panelHigh, padding: 13 },
  messageAuthorLine: { flexDirection: 'row', alignItems: 'center', gap: 6, marginBottom: 6 },
  messageAuthor: { color: colors.muted, fontSize: 10, fontWeight: '700' },
  messageTime: { color: colors.faint, fontSize: 9 },
  userMessageText: { color: colors.text, fontSize: 13, lineHeight: 20 },
  assistantMessage: { paddingHorizontal: 3, paddingVertical: 4 },
  assistantAvatar: { width: 22, height: 22, borderRadius: 8, backgroundColor: colors.accent, alignItems: 'center', justifyContent: 'center' },
  assistantAuthor: { color: colors.text, fontSize: 10, fontWeight: '800' },
  assistantMessageText: { color: colors.textSoft, fontSize: 14, lineHeight: 22 },
  streamingDot: { width: 7, height: 7, borderRadius: 4, backgroundColor: colors.accent },
  errorCard: { flexDirection: 'row', gap: 10, borderRadius: radius.md, backgroundColor: colors.redSoft, padding: 12 },
  errorTitle: { color: colors.red, fontSize: 12, fontWeight: '700' },
  errorDetail: { color: colors.textSoft, fontSize: 11, marginTop: 2 },
  composerWrap: { paddingHorizontal: 12, paddingTop: 8, paddingBottom: 10, backgroundColor: colors.canvas },
  composer: { borderRadius: 18, borderWidth: 1, borderColor: colors.borderStrong, backgroundColor: colors.panel, paddingHorizontal: 12, paddingTop: 10, paddingBottom: 8, ...shadow },
  composerInput: { minHeight: 42, maxHeight: 112, color: colors.text, fontSize: 14, lineHeight: 20, outlineStyle: 'none' } as never,
  composerActions: { minHeight: 36, flexDirection: 'row', alignItems: 'center', gap: 8 },
  composerIconButton: { width: 36, height: 36, borderRadius: 11, alignItems: 'center', justifyContent: 'center' },
  composerMode: { flexDirection: 'row', alignItems: 'center', gap: 4, paddingHorizontal: 8, height: 30, borderRadius: 9, backgroundColor: colors.panelHigh },
  composerModeText: { color: colors.textSoft, fontSize: 10, fontWeight: '700' },
  sendButton: { marginLeft: 'auto', width: 34, height: 34, borderRadius: 11, alignItems: 'center', justifyContent: 'center', backgroundColor: colors.accent },
  sendButtonDisabled: { backgroundColor: colors.panelHigh },
  composerHint: { color: colors.faint, fontSize: 9, textAlign: 'center', marginTop: 6 },
  sessionHeader: { minHeight: 68, flexDirection: 'row', alignItems: 'center', gap: 10, paddingHorizontal: 12, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: colors.border, backgroundColor: colors.canvasSoft },
  headerIconButton: { width: 40, height: 40, borderRadius: 12, alignItems: 'center', justifyContent: 'center' },
  sessionHeaderCopy: { flex: 1, minWidth: 0 },
  sessionHeaderTitle: { color: colors.text, fontSize: 14, lineHeight: 19, fontWeight: '700' },
  sessionHeaderMeta: { flexDirection: 'row', alignItems: 'center', gap: 6, marginTop: 4 },
  sessionHeaderMetaText: { color: colors.muted, fontSize: 10 },
  reviewRail: { width: 290, borderLeftWidth: StyleSheet.hairlineWidth, borderLeftColor: colors.border, backgroundColor: colors.canvasSoft, padding: 18 },
  reviewRailTitle: { color: colors.text, fontSize: 17, fontWeight: '700', marginTop: 6, marginBottom: 13 },
  reviewMetric: { flexDirection: 'row', alignItems: 'center', gap: 8, minHeight: 28 },
  reviewMetricLabel: { color: colors.textSoft, fontSize: 11 },
  reviewDivider: { height: StyleSheet.hairlineWidth, backgroundColor: colors.border, marginVertical: 16 },
  changeSummary: { flexDirection: 'row', justifyContent: 'space-between', marginTop: 10, marginBottom: 14 },
  changeNumber: { color: colors.text, fontSize: 18, fontWeight: '800' },
  changeLabel: { color: colors.muted, fontSize: 9, marginTop: 2 },
  changedFile: { minHeight: 38, flexDirection: 'row', alignItems: 'center', gap: 8 },
  changedFileName: { flex: 1, color: colors.textSoft, fontSize: 10 },
  changedFileStats: { color: colors.muted, fontSize: 9 },
  abortButton: { minHeight: 42, flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 8, borderRadius: radius.md, borderWidth: 1, borderColor: '#573036', backgroundColor: colors.redSoft },
  abortButtonText: { color: colors.red, fontSize: 11, fontWeight: '700' },
  terminalPanel: { borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.terminal, overflow: 'hidden', ...shadow },
  terminalTopbar: { minHeight: 44, flexDirection: 'row', alignItems: 'center', paddingHorizontal: 13, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: colors.border, backgroundColor: '#0f1115' },
  terminalLights: { flexDirection: 'row', gap: 6 },
  terminalLight: { width: 9, height: 9, borderRadius: 5 },
  terminalTitle: { flex: 1, flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 6 },
  terminalTitleText: { color: colors.muted, fontSize: 10, fontWeight: '600' },
  terminalBadge: { color: colors.amber, fontSize: 8, fontWeight: '700', textTransform: 'uppercase' },
  terminalBody: { minHeight: 238, maxHeight: 310, padding: 14 },
  terminalLine: { color: '#aab3bf', fontFamily: 'monospace', fontSize: 11, lineHeight: 20 },
  cursor: { width: 8, height: 15, backgroundColor: colors.accent, marginTop: 3 },
});
