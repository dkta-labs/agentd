import { StatusBar } from 'expo-status-bar';
import React from 'react';
import { StyleSheet, View } from 'react-native';
import { SafeAreaProvider, SafeAreaView } from 'react-native-safe-area-context';

import { LabSwitcher, SourceNote } from './src/components';
import type { Candidate } from './src/model';
import { FleetScreen, FocusScreen, OpsScreen } from './src/screens';
import { colors } from './src/theme';

export default function App() {
  const [candidate, setCandidate] = React.useState<Candidate>('fleet');

  return (
    <SafeAreaProvider>
      <StatusBar style="light" backgroundColor={colors.canvasSoft} />
      <SafeAreaView style={styles.safeArea} edges={['top', 'right', 'bottom', 'left']}>
        <LabSwitcher candidate={candidate} onChange={setCandidate} />
        <View style={styles.content}>
          {candidate === 'fleet' ? <FleetScreen onOpenSession={() => setCandidate('focus')} /> : null}
          {candidate === 'focus' ? <FocusScreen onBack={() => setCandidate('fleet')} /> : null}
          {candidate === 'ops' ? <OpsScreen /> : null}
        </View>
        <SourceNote>Interaction lab only · synthetic agentd fixtures · no backend connection</SourceNote>
      </SafeAreaView>
    </SafeAreaProvider>
  );
}

const styles = StyleSheet.create({
  safeArea: {
    flex: 1,
    backgroundColor: colors.canvas,
  },
  content: {
    flex: 1,
    minHeight: 0,
  },
});
