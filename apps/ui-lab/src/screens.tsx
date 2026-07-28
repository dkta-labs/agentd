import {
  Activity,
  Bell,
  Bot,
  CheckCircle2,
  ChevronRight,
  CircleDot,
  Cloud,
  Command,
  Cpu,
  Database,
  GitBranch,
  GitCommit,
  Globe2,
  Network,
  Plus,
  Radio,
  RefreshCw,
  Rocket,
  Search,
  ServerCog,
  ShieldCheck,
  SlidersHorizontal,
  Terminal,
  Timer,
  Wifi,
} from 'lucide-react-native';
import React from 'react';
import { ScrollView, StyleSheet, Text, useWindowDimensions, View } from 'react-native';

import {
  Composer,
  Eyebrow,
  NodeCard,
  PressableScale,
  ReviewRail,
  SectionTitle,
  SessionHeader,
  SessionPill,
  SessionRow,
  TerminalPanel,
  Timeline,
} from './components';
import { nodeFor, nodes, sessions, terminalLines, workspaceFor } from './model';
import { colors, radius, shadow, type } from './theme';

const primarySession = sessions[0]!;
const primaryWorkspace = workspaceFor(primarySession.workspaceId);
const primaryNode = nodeFor(primarySession.nodeId);

export function FleetScreen({ onOpenSession }: { onOpenSession: () => void }) {
  const { width } = useWindowDimensions();
  const desktop = width >= 980;
  const mobile = width < 620;
  const [selectedNode, setSelectedNode] = React.useState('studio');
  const selected = nodeFor(selectedNode);
  const filteredSessions = sessions.filter((session) => session.nodeId === selectedNode);
  const attention = sessions.find((session) => session.state === 'needs-input')!;

  return (
    <ScrollView
      style={styles.screen}
      contentContainerStyle={[styles.fleetContent, desktop && styles.fleetContentDesktop]}
      showsVerticalScrollIndicator={false}
    >
      <View style={[styles.hero, mobile && styles.heroMobile]}>
        <View style={styles.heroCopy}>
          <Eyebrow>CONTROL PLANE</Eyebrow>
          <Text style={styles.heroTitle}>Your agents, in one place.</Text>
          <Text style={styles.heroSubtitle}>Two nodes are online. One run needs a decision.</Text>
        </View>
        <View style={[styles.heroActions, mobile && styles.heroActionsMobile]}>
          <PressableScale accessibilityLabel="Search sessions" style={styles.squareButton}>
            <Search size={19} color={colors.textSoft} />
          </PressableScale>
          <PressableScale accessibilityLabel="View notifications" style={styles.squareButton}>
            <Bell size={19} color={colors.textSoft} />
            <View style={styles.notificationDot} />
          </PressableScale>
          <PressableScale accessibilityLabel="Start new run" style={styles.primaryButton}>
            <Plus size={18} color={colors.accentInk} strokeWidth={2.8} />
            <Text style={styles.primaryButtonText}>New run</Text>
          </PressableScale>
        </View>
      </View>

      <View style={styles.metricStrip}>
        <View style={styles.metricItem}><View style={[styles.metricIcon, { backgroundColor: colors.greenSoft }]}><Wifi size={16} color={colors.green} /></View><View><Text style={styles.metricValue}>2 / 3</Text><Text style={styles.metricLabel}>nodes online</Text></View></View>
        <View style={styles.metricDivider} />
        <View style={styles.metricItem}><View style={[styles.metricIcon, { backgroundColor: '#26351b' }]}><Activity size={16} color={colors.accent} /></View><View><Text style={styles.metricValue}>2</Text><Text style={styles.metricLabel}>active runs</Text></View></View>
        <View style={styles.metricDivider} />
        <View style={styles.metricItem}><View style={[styles.metricIcon, { backgroundColor: colors.amberSoft }]}><CircleDot size={16} color={colors.amber} /></View><View><Text style={styles.metricValue}>1</Text><Text style={styles.metricLabel}>needs you</Text></View></View>
      </View>

      <SectionTitle title="Machines" count="3" action="Manage" />
      <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={styles.nodeList}>
        {nodes.map((node) => (
          <NodeCard key={node.id} node={node} selected={node.id === selectedNode} onPress={() => setSelectedNode(node.id)} />
        ))}
      </ScrollView>

      <View style={[styles.fleetColumns, desktop && styles.fleetColumnsDesktop]}>
        <View style={styles.fleetMainColumn}>
          <SectionTitle title="Needs you" count="1" />
          <SessionRow session={attention} emphasized onPress={onOpenSession} />

          <View style={styles.sectionGap} />
          <SectionTitle title={`${selected.name} sessions`} count={String(filteredSessions.length)} action="All sessions" />
          <View style={styles.sessionList}>
            {filteredSessions.length > 0 ? filteredSessions.map((session) => <SessionRow key={session.id} session={session} onPress={onOpenSession} />) : (
              <View style={styles.emptyCard}>
                <CheckCircle2 size={21} color={colors.green} />
                <View><Text style={styles.emptyTitle}>No sessions on this node</Text><Text style={styles.emptyCopy}>It is ready for a new run.</Text></View>
              </View>
            )}
          </View>
        </View>

        <View style={styles.fleetSideColumn}>
          <SectionTitle title="Live now" />
          <View style={styles.liveCard}>
            <View style={styles.liveCardTop}>
              <View style={styles.liveAvatar}><Bot size={20} color={colors.accentInk} /></View>
              <View style={styles.liveTitleCopy}>
                <Text style={styles.liveWorkspace}>agentd · main</Text>
                <Text style={styles.liveNode}>Studio · OMP</Text>
              </View>
              <SessionPill state="working" compact />
            </View>
            <Text style={styles.liveTitle}>Make controller restart recovery safe</Text>
            <Text style={styles.liveSummary}>The safe default is worker-authoritative only after identity and monotonic sequence checks pass.</Text>
            <View style={styles.liveProgress}><View style={styles.liveProgressFill} /></View>
            <View style={styles.liveFooter}>
              <View style={styles.liveMeta}><Timer size={13} color={colors.muted} /><Text style={styles.liveMetaText}>3m 18s</Text></View>
              <View style={styles.liveMeta}><GitCommit size={13} color={colors.muted} /><Text style={styles.liveMetaText}>3 files</Text></View>
              <PressableScale accessibilityLabel="Open live session" onPress={onOpenSession} style={styles.liveOpen}>
                <Text style={styles.liveOpenText}>Open</Text><ChevronRight size={14} color={colors.accentInk} />
              </PressableScale>
            </View>
          </View>

          <View style={styles.sectionGap} />
          <SectionTitle title="Workspace replicas" />
          <View style={styles.replicaCard}>
            <View style={styles.replicaHeader}><View><Text style={styles.replicaName}>agentd</Text><Text style={styles.replicaBranch}>main · 1b21416</Text></View><GitBranch size={18} color={colors.cyan} /></View>
            <View style={styles.replicaRow}><View style={[styles.replicaDot, { backgroundColor: colors.green }]} /><Text style={styles.replicaNode}>Studio</Text><Text style={styles.replicaState}>dirty · +2 commits</Text></View>
            <View style={styles.replicaRow}><View style={[styles.replicaDot, { backgroundColor: colors.green }]} /><Text style={styles.replicaNode}>Edge VPS</Text><Text style={styles.replicaState}>clean · current</Text></View>
            <View style={styles.replicaRow}><View style={[styles.replicaDot, { backgroundColor: colors.red }]} /><Text style={styles.replicaNode}>Laptop</Text><Text style={styles.replicaState}>offline · behind 1</Text></View>
          </View>
        </View>
      </View>
    </ScrollView>
  );
}

export function FocusScreen({ onBack }: { onBack: () => void }) {
  const { width } = useWindowDimensions();
  const desktop = width >= 1060;
  const [messages, setMessages] = React.useState<string[]>([]);

  return (
    <View style={styles.focusScreen}>
      <SessionHeader session={primarySession} onBack={onBack} />
      <View style={styles.focusBody}>
        <View style={styles.conversationColumn}>
          <ScrollView
            style={styles.conversationScroll}
            contentContainerStyle={styles.conversationContent}
            showsVerticalScrollIndicator={false}
          >
            <View style={styles.runBanner}>
              <View style={styles.runBannerIcon}><Radio size={16} color={colors.accent} /></View>
              <View style={styles.runBannerCopy}><Text style={styles.runBannerTitle}>OMP is working on Studio</Text><Text style={styles.runBannerMeta}>main · 3 files changed · sequence 184</Text></View>
              <Text style={styles.runBannerTime}>3:18</Text>
            </View>
            <Timeline extraMessages={messages} />
          </ScrollView>
          <Composer onSend={(message) => setMessages((current) => [...current, message])} />
        </View>
        {desktop ? <ReviewRail session={primarySession} workspace={primaryWorkspace} /> : null}
      </View>
    </View>
  );
}

export function OpsScreen() {
  const { width } = useWindowDimensions();
  const desktop = width >= 900;
  const [command, setCommand] = React.useState<string>();
  const commands = [
    { label: 'Reconnect', value: 'agentd node reconnect', icon: RefreshCw },
    { label: 'Health', value: 'agentd node health --json', icon: Activity },
    { label: 'Logs', value: 'agentd node logs --follow', icon: Terminal },
    { label: 'Restart', value: 'agentd node restart --graceful', icon: Rocket },
  ];

  return (
    <ScrollView style={styles.screen} contentContainerStyle={[styles.opsContent, desktop && styles.opsContentDesktop]} showsVerticalScrollIndicator={false}>
      <View style={styles.opsHeader}>
        <View>
          <Eyebrow>OPERATIONS SURFACE</Eyebrow>
          <Text style={styles.opsTitle}>Studio</Text>
          <View style={styles.opsSubtitle}><Radio size={13} color={colors.green} /><Text style={styles.opsSubtitleText}>Online · 24 ms · seen now</Text></View>
        </View>
        <View style={styles.opsHeaderActions}>
          <PressableScale accessibilityLabel="Open node settings" style={styles.squareButton}><SlidersHorizontal size={19} color={colors.textSoft} /></PressableScale>
          <PressableScale accessibilityLabel="Restart node" onPress={() => setCommand('agentd node restart --graceful')} style={styles.secondaryButton}><RefreshCw size={17} color={colors.textSoft} /><Text style={styles.secondaryButtonText}>Restart</Text></PressableScale>
        </View>
      </View>

      <View style={styles.opsMetrics}>
        <View style={styles.opsMetric}><Cpu size={18} color={colors.cyan} /><View><Text style={styles.opsMetricValue}>18%</Text><Text style={styles.opsMetricLabel}>CPU</Text></View><View style={styles.sparkBars}>{[5, 9, 7, 12, 8, 15, 10].map((height, index) => <View key={index} style={[styles.sparkBar, { height }]} />)}</View></View>
        <View style={styles.opsMetric}><Database size={18} color={colors.purple} /><View><Text style={styles.opsMetricValue}>4.7 GB</Text><Text style={styles.opsMetricLabel}>Memory</Text></View></View>
        <View style={styles.opsMetric}><Activity size={18} color={colors.accent} /><View><Text style={styles.opsMetricValue}>2</Text><Text style={styles.opsMetricLabel}>Active runs</Text></View></View>
        <View style={styles.opsMetric}><Network size={18} color={colors.green} /><View><Text style={styles.opsMetricValue}>184</Text><Text style={styles.opsMetricLabel}>Last sequence</Text></View></View>
      </View>

      <View style={[styles.opsColumns, desktop && styles.opsColumnsDesktop]}>
        <View style={styles.terminalColumn}>
          <SectionTitle title="Live screen" action="Open full terminal" />
          <TerminalPanel lines={terminalLines} command={command} />
          <Text style={styles.terminalWarning}>Degraded fallback only. Prefer normalized session events for agent work.</Text>

          <View style={styles.sectionGap} />
          <SectionTitle title="Quick actions" />
          <View style={styles.quickGrid}>
            {commands.map((item) => {
              const Icon = item.icon;
              return (
                <PressableScale key={item.label} accessibilityLabel={item.label} testID={`ops-${item.label.toLowerCase()}`} onPress={() => setCommand(item.value)} style={styles.quickAction}>
                  <View style={styles.quickActionIcon}><Icon size={18} color={colors.cyan} /></View>
                  <Text style={styles.quickActionLabel}>{item.label}</Text>
                  <ChevronRight size={15} color={colors.faint} />
                </PressableScale>
              );
            })}
          </View>
        </View>

        <View style={styles.nodeDetailColumn}>
          <SectionTitle title="Node detail" />
          <View style={styles.detailCard}>
            <View style={styles.detailHero}><View style={styles.detailHeroIcon}><ServerCog size={23} color={colors.accentInk} /></View><View><Text style={styles.detailNodeName}>{primaryNode.name}</Text><Text style={styles.detailNodeLocation}>{primaryNode.location}</Text></View></View>
            <View style={styles.detailDivider} />
            <DetailRow icon={Cloud} label="Controller" value="edge.dkta.dev" />
            <DetailRow icon={Globe2} label="Platform" value={primaryNode.platform} />
            <DetailRow icon={Command} label="Harness" value={primaryNode.version} />
            <DetailRow icon={GitBranch} label="Workspaces" value="2 registered" />
            <DetailRow icon={ShieldCheck} label="Identity" value="Verified" good />
          </View>

          <View style={styles.sectionGap} />
          <SectionTitle title="Recent events" />
          <View style={styles.eventCard}>
            <EventRow tone="good" title="Session reconciled" detail="restart-recovery · sequence 184" at="now" />
            <EventRow tone="good" title="Controller restored" detail="mutual identity verified" at="1s" />
            <EventRow tone="warn" title="Controller lost" detail="retrying outbound connection" at="3s" />
            <EventRow tone="neutral" title="Heartbeat accepted" detail="2 workspaces · 1 active run" at="17s" last />
          </View>
        </View>
      </View>
    </ScrollView>
  );
}

function DetailRow({ icon: Icon, label, value, good = false }: { icon: typeof Cloud; label: string; value: string; good?: boolean }) {
  return (
    <View style={styles.detailRow}>
      <Icon size={15} color={colors.muted} />
      <Text style={styles.detailLabel}>{label}</Text>
      <Text style={[styles.detailValue, good && { color: colors.green }]}>{value}</Text>
    </View>
  );
}

function EventRow({ tone, title, detail, at, last = false }: { tone: 'good' | 'warn' | 'neutral'; title: string; detail: string; at: string; last?: boolean }) {
  const color = tone === 'good' ? colors.green : tone === 'warn' ? colors.amber : colors.muted;
  return (
    <View style={[styles.eventRow, last && styles.eventRowLast]}>
      <View style={[styles.eventDot, { backgroundColor: color }]} />
      <View style={styles.eventCopy}><Text style={styles.eventTitle}>{title}</Text><Text style={styles.eventDetail}>{detail}</Text></View>
      <Text style={styles.eventTime}>{at}</Text>
    </View>
  );
}

const styles = StyleSheet.create({
  screen: { flex: 1, backgroundColor: colors.canvas },
  fleetContent: { paddingHorizontal: 14, paddingTop: 18, paddingBottom: 44, gap: 14 },
  fleetContentDesktop: { width: '100%', maxWidth: 1260, alignSelf: 'center', paddingHorizontal: 28, paddingTop: 28, gap: 18 },
  hero: { flexDirection: 'row', alignItems: 'flex-end', justifyContent: 'space-between', gap: 14 },
  heroMobile: { flexDirection: 'column', alignItems: 'stretch', gap: 12 },
  heroCopy: { flex: 1 },
  heroTitle: { ...type.title, fontSize: 28, lineHeight: 34, marginTop: 4 },
  heroSubtitle: { ...type.body, color: colors.muted, marginTop: 5 },
  heroActions: { flexDirection: 'row', alignItems: 'center', gap: 8 },
  heroActionsMobile: { justifyContent: 'flex-end' },
  squareButton: { position: 'relative', width: 42, height: 42, alignItems: 'center', justifyContent: 'center', borderRadius: 13, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel },
  notificationDot: { position: 'absolute', right: 9, top: 8, width: 6, height: 6, borderRadius: 3, backgroundColor: colors.amber, borderWidth: 1, borderColor: colors.panel },
  primaryButton: { minHeight: 42, flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 7, borderRadius: 13, paddingHorizontal: 14, backgroundColor: colors.accent },
  primaryButtonText: { color: colors.accentInk, fontSize: 12, fontWeight: '800' },
  secondaryButton: { minHeight: 42, flexDirection: 'row', alignItems: 'center', justifyContent: 'center', gap: 7, borderRadius: 13, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, paddingHorizontal: 14 },
  secondaryButtonText: { color: colors.textSoft, fontSize: 12, fontWeight: '700' },
  metricStrip: { minHeight: 82, flexDirection: 'row', alignItems: 'center', borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, paddingHorizontal: 16, ...shadow },
  metricItem: { flex: 1, flexDirection: 'row', alignItems: 'center', gap: 10 },
  metricIcon: { width: 34, height: 34, borderRadius: 11, alignItems: 'center', justifyContent: 'center' },
  metricValue: { color: colors.text, fontSize: 16, fontWeight: '800' },
  metricLabel: { color: colors.muted, fontSize: 9, marginTop: 1 },
  metricDivider: { width: StyleSheet.hairlineWidth, height: 38, backgroundColor: colors.border, marginHorizontal: 12 },
  nodeList: { gap: 10, paddingBottom: 4 },
  fleetColumns: { gap: 20 },
  fleetColumnsDesktop: { flexDirection: 'row', alignItems: 'flex-start' },
  fleetMainColumn: { flex: 1, gap: 10 },
  fleetSideColumn: { flex: 0.72, minWidth: 320, gap: 10 },
  sectionGap: { height: 7 },
  sessionList: { gap: 9 },
  emptyCard: { minHeight: 84, flexDirection: 'row', alignItems: 'center', gap: 12, borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, padding: 15 },
  emptyTitle: { color: colors.text, fontSize: 13, fontWeight: '700' },
  emptyCopy: { color: colors.muted, fontSize: 11, marginTop: 2 },
  liveCard: { borderRadius: radius.lg, borderWidth: 1, borderColor: '#45552b', backgroundColor: colors.panel, padding: 15, ...shadow },
  liveCardTop: { flexDirection: 'row', alignItems: 'center', gap: 10 },
  liveAvatar: { width: 38, height: 38, borderRadius: 12, alignItems: 'center', justifyContent: 'center', backgroundColor: colors.accent },
  liveTitleCopy: { flex: 1 },
  liveWorkspace: { color: colors.text, fontSize: 12, fontWeight: '700' },
  liveNode: { color: colors.muted, fontSize: 9, marginTop: 2 },
  liveTitle: { color: colors.text, fontSize: 15, lineHeight: 21, fontWeight: '700', marginTop: 15 },
  liveSummary: { color: colors.muted, fontSize: 11, lineHeight: 17, marginTop: 5 },
  liveProgress: { height: 3, borderRadius: 2, backgroundColor: colors.panelHigh, marginTop: 14, overflow: 'hidden' },
  liveProgressFill: { width: '68%', height: '100%', backgroundColor: colors.accent },
  liveFooter: { minHeight: 42, flexDirection: 'row', alignItems: 'flex-end', gap: 12, marginTop: 5 },
  liveMeta: { flexDirection: 'row', alignItems: 'center', gap: 5 },
  liveMetaText: { color: colors.muted, fontSize: 9 },
  liveOpen: { marginLeft: 'auto', minHeight: 34, flexDirection: 'row', alignItems: 'center', gap: 3, borderRadius: 10, backgroundColor: colors.accent, paddingHorizontal: 11 },
  liveOpenText: { color: colors.accentInk, fontSize: 10, fontWeight: '800' },
  replicaCard: { borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, padding: 14 },
  replicaHeader: { flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', paddingBottom: 12 },
  replicaName: { color: colors.text, fontSize: 14, fontWeight: '700' },
  replicaBranch: { color: colors.muted, fontSize: 9, marginTop: 2 },
  replicaRow: { minHeight: 38, flexDirection: 'row', alignItems: 'center', gap: 8, borderTopWidth: StyleSheet.hairlineWidth, borderTopColor: colors.border },
  replicaDot: { width: 7, height: 7, borderRadius: 4 },
  replicaNode: { flex: 1, color: colors.textSoft, fontSize: 11 },
  replicaState: { color: colors.muted, fontSize: 9 },
  focusScreen: { flex: 1, backgroundColor: colors.canvas },
  focusBody: { flex: 1, flexDirection: 'row', minHeight: 0 },
  conversationColumn: { flex: 1, minWidth: 0 },
  conversationScroll: { flex: 1 },
  conversationContent: { width: '100%', maxWidth: 760, alignSelf: 'center', paddingHorizontal: 14, paddingTop: 14, paddingBottom: 22 },
  runBanner: { minHeight: 62, flexDirection: 'row', alignItems: 'center', gap: 10, borderRadius: radius.md, borderWidth: 1, borderColor: '#3a4729', backgroundColor: '#12170e', paddingHorizontal: 12, marginBottom: 14 },
  runBannerIcon: { width: 32, height: 32, borderRadius: 10, alignItems: 'center', justifyContent: 'center', backgroundColor: '#26351b' },
  runBannerCopy: { flex: 1 },
  runBannerTitle: { color: colors.text, fontSize: 12, fontWeight: '700' },
  runBannerMeta: { color: colors.muted, fontSize: 9, marginTop: 3 },
  runBannerTime: { color: colors.accent, fontSize: 10, fontWeight: '700' },
  opsContent: { paddingHorizontal: 14, paddingTop: 18, paddingBottom: 44, gap: 17 },
  opsContentDesktop: { width: '100%', maxWidth: 1260, alignSelf: 'center', paddingHorizontal: 28, paddingTop: 28 },
  opsHeader: { flexDirection: 'row', alignItems: 'flex-end', justifyContent: 'space-between', gap: 12 },
  opsTitle: { ...type.title, fontSize: 28, lineHeight: 34, marginTop: 3 },
  opsSubtitle: { flexDirection: 'row', alignItems: 'center', gap: 6, marginTop: 5 },
  opsSubtitleText: { color: colors.muted, fontSize: 11 },
  opsHeaderActions: { flexDirection: 'row', gap: 8 },
  opsMetrics: { flexDirection: 'row', flexWrap: 'wrap', gap: 9 },
  opsMetric: { flexGrow: 1, flexBasis: 180, minHeight: 72, flexDirection: 'row', alignItems: 'center', gap: 11, borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, paddingHorizontal: 14 },
  opsMetricValue: { color: colors.text, fontSize: 15, fontWeight: '800' },
  opsMetricLabel: { color: colors.muted, fontSize: 9, marginTop: 2 },
  sparkBars: { marginLeft: 'auto', height: 18, flexDirection: 'row', alignItems: 'flex-end', gap: 2 },
  sparkBar: { width: 3, borderRadius: 2, backgroundColor: colors.cyan },
  opsColumns: { gap: 20 },
  opsColumnsDesktop: { flexDirection: 'row', alignItems: 'flex-start' },
  terminalColumn: { flex: 1.25, gap: 10 },
  nodeDetailColumn: { flex: 0.75, minWidth: 320, gap: 10 },
  terminalWarning: { color: colors.amber, fontSize: 9, lineHeight: 14, textAlign: 'center' },
  quickGrid: { flexDirection: 'row', flexWrap: 'wrap', gap: 8 },
  quickAction: { flexGrow: 1, flexBasis: 150, minHeight: 54, flexDirection: 'row', alignItems: 'center', gap: 9, borderRadius: radius.md, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, paddingHorizontal: 11 },
  quickActionIcon: { width: 32, height: 32, borderRadius: 10, alignItems: 'center', justifyContent: 'center', backgroundColor: colors.cyanSoft },
  quickActionLabel: { flex: 1, color: colors.textSoft, fontSize: 11, fontWeight: '700' },
  detailCard: { borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, padding: 15, ...shadow },
  detailHero: { flexDirection: 'row', alignItems: 'center', gap: 11 },
  detailHeroIcon: { width: 44, height: 44, borderRadius: 14, alignItems: 'center', justifyContent: 'center', backgroundColor: colors.accent },
  detailNodeName: { color: colors.text, fontSize: 15, fontWeight: '700' },
  detailNodeLocation: { color: colors.muted, fontSize: 10, marginTop: 3 },
  detailDivider: { height: StyleSheet.hairlineWidth, backgroundColor: colors.border, marginVertical: 14 },
  detailRow: { minHeight: 38, flexDirection: 'row', alignItems: 'center', gap: 9 },
  detailLabel: { flex: 1, color: colors.muted, fontSize: 10 },
  detailValue: { color: colors.textSoft, fontSize: 10, fontWeight: '600' },
  eventCard: { borderRadius: radius.lg, borderWidth: 1, borderColor: colors.border, backgroundColor: colors.panel, paddingHorizontal: 14 },
  eventRow: { minHeight: 60, flexDirection: 'row', alignItems: 'center', gap: 10, borderBottomWidth: StyleSheet.hairlineWidth, borderBottomColor: colors.border },
  eventRowLast: { borderBottomWidth: 0 },
  eventDot: { width: 8, height: 8, borderRadius: 4 },
  eventCopy: { flex: 1 },
  eventTitle: { color: colors.textSoft, fontSize: 11, fontWeight: '600' },
  eventDetail: { color: colors.muted, fontSize: 9, marginTop: 3 },
  eventTime: { color: colors.faint, fontSize: 9 },
});
